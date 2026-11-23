#!/usr/bin/env python3
"""Observe file cache over the restored VM's existing telemetry socket."""
import json
import os
from pathlib import Path
import runpy
import sys
import time


POINTS = {('session-start',0),('post-tool',10),('post-tool',12),('pre-tool',14),
          ('post-tool',14),('post-tool',15),('session-end',0)}


def ram_census(api):
    from idle_hook import select_slots
    from vmm_census import census

    pid,ticks = api.identity
    slot_log = Path(os.environ['ZB_SLOT_EVENTS']).read_text()
    ram,other = select_slots(slot_log,pid,ticks,1_000_000_000//os.sysconf('SC_CLK_TCK'),8<<30)
    assert not other
    result = census(pid,ram,other)
    assert 'guest_ram' in result['categories'] and 'mixed_vma' not in result['categories']
    return result | dict(ram_slots=ram)


def main():
    phase, runtime, step, socket = sys.argv[1:]
    if (phase,int(step)) not in POINTS:
        return
    begin = time.monotonic_ns()
    sys.path.insert(0,os.environ['ZB_CACHE_TRACE_ASSETS'])
    from idle_hook import Firecracker
    from guest_sample import snapshot

    api = Firecracker(socket)
    api.inspect()
    assert api.vm_id == runtime
    pid, ticks = api.identity
    vsock = api.path.with_name('firecracker.vsock')
    result = snapshot(vsock)
    assert result['guest']['torch_file_cache']['task_root'] == f'/container/{runtime}/rootfs'
    pss_kib, = [int(line.split()[1]) for line in Path(f'/proc/{pid}/smaps_rollup').read_text().splitlines()
                if line.startswith('Pss:')]
    extra = {}
    if os.environ.get('ZB_RELEASE_TRANSPORT'):
        extra['host_guest_ram_census'] = ram_census(api)
        if (phase,int(step)) == ('post-tool',12):
            trace = Path(os.environ['ZB_RELEASE_HANDLER_EVENTS'])
            assert trace.read_text().startswith('ready '), 'free-page reporting capture must be armed before release'
            probe = runpy.run_path(os.environ['ZB_RELEASE_TRANSPORT'])['snapshot']
            extra['wheel_release'] = probe(vsock,request=b'evictwhl\n')
            extra['host_guest_ram_after_release'] = ram_census(api)
    end = time.monotonic_ns()
    row = dict(phase=phase,step=int(step),runtime=runtime,vmm_pid=pid,vmm_start_ticks=int(ticks),
               vmm_pss_kib_after_guest_query=pss_kib,host_observer_begin_ns=begin,host_observer_end_ns=end,**result,**extra)
    with Path(os.environ['ZB_CACHE_OUT']).open('a') as f:
        print(json.dumps(row,separators=(',',':')),file=f)


if __name__ == '__main__':
    main()
