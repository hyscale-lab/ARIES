#!/usr/bin/env python3
"""Collect full AgentENV page windows at ARIES transitions and timer boundaries."""
import argparse
from collections import Counter
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
from select import select
import signal
import socket
import struct
import subprocess
import sys
import time

import dirty as kvm_dirty
STAGE = Path(__file__).resolve().parent

def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()

sys.path.insert(0, str(STAGE.parent))
from idle import start_identity
from idle_hook import (Firecracker, INITIAL_PID_NAMESPACE, guest_snapshot, select_slots,
                       signal_error, snapshot_path)
from snapshot_probe import PAGE, PFN_MASK, PRESENT, sample_snapshot, selected
from vmm_census import covered, mappings


def capture_content(pid, samples):
    rows = []
    with open(f'/proc/{pid}/mem', 'rb', buffering=0) as mem, open(f'/proc/{pid}/pagemap', 'rb', buffering=0) as pm:
        for gfn, addr, _ in samples:
            offset = addr // PAGE * 8
            entry, = struct.unpack('=Q', os.pread(pm.fileno(), 8, offset))
            if not entry & PRESENT:
                state, data = 'absent', None
            elif not entry & PFN_MASK:
                state, data = 'masked_or_zero', None
            else:
                data = os.pread(mem.fileno(), PAGE, addr)
                later, = struct.unpack('=Q', os.pread(pm.fileno(), 8, offset))
                if len(data) != PAGE or (later & (PRESENT | PFN_MASK)) != (entry & (PRESENT | PFN_MASK)):
                    state, data = 'mapping_changed', None
                else:
                    state = 'resident'
            rows.append((gfn, state, data))
    return rows


def content_digests(rows):
    return [dict(gfn=gfn, state=state, **({'digest': hashlib.sha256(data).hexdigest()} if data else {}))
            for gfn, state, data in rows]


def capture_census(pid, ram, other, snapshot):
    begin = time.monotonic_ns()
    proc = Path('/proc') / str(pid)
    info = snapshot.stat()
    snapshot_key = info.st_dev, info.st_ino
    rows, ptes = [], []
    with (proc / 'pagemap').open('rb', buffering=0) as pm:
        for mapping in mappings((proc / 'smaps').read_text()):
            lo, hi = mapping['lo'], mapping['hi']
            mem, device = covered(lo, hi, ram), covered(lo, hi, other)
            if mem == hi - lo:
                role = 'guest_ram'
            elif device == hi - lo:
                role = 'device_slot'
            else:
                role = 'mixed_vma' if mem or device else 'outside_kvm'
            if role == 'guest_ram' and mapping['mode'].endswith('p') and snapshot_key == (mapping['device'], mapping['inode']):
                role = 'guest_ram_snapshot_vma'
                for start in range(lo, hi, 8192 * PAGE):
                    size = (min(hi, start + 8192 * PAGE) - start) // PAGE * 8
                    raw = os.pread(pm.fileno(), size, start // PAGE * 8)
                    if len(raw) != size:
                        raise OSError('short snapshot pagemap read')
                    ptes.append(raw)
            rows.append((role, mapping))
    return begin, time.monotonic_ns(), rows, ptes


def census_counts(begin, end, rows, ptes):
    totals = {}
    for role, row in rows:
        counts = totals.setdefault(role, Counter())
        counts['vmas'] += 1
        for field in ('Rss', 'Pss', 'Anonymous', 'Private_Dirty'):
            counts[field + '_bytes'] += row[field]
    file_pages = sum(entry & PRESENT != 0 and entry & (1 << 61) != 0
                     for raw in ptes for (entry,) in struct.iter_unpack('=Q', raw))
    verified = 'guest_ram_snapshot_vma' in totals
    return dict(begin_ns=begin, end_ns=end, categories=totals,
                current_file_backed_snapshot_rss_bytes=file_pages * PAGE if verified else None,
                snapshot_inode_verified=verified, decode_end_ns=time.monotonic_ns())


def exchange(child, command, expected):
    if command:
        child.stdin.write(command + '\n')
        child.stdin.flush()
    if not select([child.stdout], [], [], 20)[0]:
        raise TimeoutError(f'native scanner did not acknowledge {command}')
    line = child.stdout.readline()
    if not line:
        raise RuntimeError(f'native scanner ended during {command}: {child.poll()}')
    reply = json.loads(line)
    if reply.pop('kind') != expected:
        raise RuntimeError(f'unexpected native scanner reply: {line}')
    return reply


def boundary(api, child, event, samples, slots, device_slots, snapshot, interior, dirty_slots, out, last_epoch):
    freeze = event['freeze'] = dict(pause_request_ns=time.monotonic_ns())
    try:
        api.request('PATCH', '/vm', 'Paused')
        freeze['pause_ack_ns'] = time.monotonic_ns()
        if interior:
            event['capture'] = exchange(child, 'capture', 'captured')
        event['content_begin_ns'] = time.monotonic_ns()
        content = capture_content(api.identity[0], samples)
        event['content_capture_end_ns'] = time.monotonic_ns()
        census = capture_census(api.identity[0], slots, device_slots, snapshot)
        event['arm'] = exchange(child, 'arm', 'armed')
        if dirty_slots:
            event['dirty_request_ns'] = time.monotonic_ns()
            dirty = kvm_dirty.read_log(api, start_identity, bitmap=True)
            event['dirty_reply_ns'] = time.monotonic_ns()
    finally:
        freeze['resume_request_ns'] = time.monotonic_ns()
        try:
            api.request('PATCH', '/vm', 'Resumed')
            freeze['resume_ack_ns'] = time.monotonic_ns()
        except Exception as exc:
            freeze['resume_error'] = str(exc)
            if api.request('GET', '/')['state'] == 'Running':
                freeze['running_confirmed_ns'] = time.monotonic_ns()
            raise
    event['export_start_ns'] = time.monotonic_ns()
    if dirty_slots:
        kvm_dirty.validate(dirty, PAGE, dirty_slots)
        started = dirty['started_us']
        if not (event['dirty_request_ns'] // 1000 <= started <= started + dirty['elapsed_us']
                <= event['dirty_reply_ns'] // 1000):
            raise RuntimeError('dirty log timestamps fall outside the request')
        if interior:
            kvm_dirty.write_bitmaps(dirty, out, event['index'], last_epoch)
        else:
            kvm_dirty.decode_bitmaps(dirty)
            for slot in dirty['slots']:
                del slot['bitmap_b64']
        event['dirty'] = dirty
    event['guest_telemetry'] = guest_snapshot(api.path.with_name('firecracker.vsock'))
    if interior:
        event['window'] = exchange(child, 'export', 'exported')
    event['content_sample'] = content_digests(content)
    event['vmm_census'] = census_counts(*census)


def collect(run, out, threads, events, runtime, api_socket):
    api = Firecracker(api_socket)
    info = api.inspect()
    pid, start = api.identity
    if api.vm_id != runtime:
        raise RuntimeError('Firecracker VM ID differs from the replay runtime')
    machine = api.request('GET', '/machine-config')
    ticks = 10**9 // os.sysconf('SC_CLK_TCK')

    def slots_now():
        return select_slots((run / 'slots.log').read_text(), pid, start, ticks, machine['mem_size_mib'] << 20)

    slots, devices = slots_now()
    if (PAGE != os.sysconf('SC_PAGE_SIZE') or not machine['track_dirty_pages']
            or any(row['flags'] != 1 for row in slots)):
        raise RuntimeError('guest RAM slots lack KVM dirty tracking')
    dirty_slots = {row['slot']: (row['guest_phys_addr'], row['memory_size']) for row in slots}
    samples = selected(slots, 8192)
    snapshot = snapshot_path(os.environ['ZB_CHECKPOINT_NAMESPACE_DIR'], runtime)
    command = [STAGE / 'native-deep-scan', out, pid, start]
    for slot in slots:
        command.extend((slot['userspace_addr'], slot['memory_size']))
    sources = [Path(__file__), STAGE / 'native-deep-scan', STAGE / 'native-deep-scan.c',
               STAGE / 'idle-scan.c', *(STAGE.parent / name for name in
               ('idle_hook.py', 'snapshot_probe.py', 'vmm_census.py', 'guest_sample.py'))]
    sources.append(STAGE / 'dirty.py')
    metadata = dict(
        schema='zepbound.native-deep-boundary.v2', scan_threads=threads, kvm_dirty=bool(dirty_slots),
        clock='host-CLOCK_MONOTONIC', runtime=runtime,
        vm_id=api.vm_id, vmm_pid=pid, vmm_start_ticks=int(start), vmm_version=info['vmm_version'],
        machine=machine, ram_slots=slots, other_kvm_slots=devices,
        evidence='one byte per 4096-byte GPA in RAM slot order, with low six bits selecting the code',
        codes=['absent at all checks', 'accessed', 'idle', 'newly-resident-unarmed', 'became-nonresident',
               'host-mapping-changed', 'pfn-masked-or-zero', 'compound-resolution-unsupported',
               'shared-or-zero-page', 'not-on-lru', 'nonexclusive-or-transition'],
        vmm_sha256=digest(Path(f'/proc/{pid}/exe')),
        dirty_format='One bit per 4 KiB slot page, least significant bit first. Initial reset discarded.',
        absent_at_arm_mask=64, absent_at_end_mask=128,
        sources={str(path): digest(path) for path in sources})
    (out / 'metadata.json').write_text(json.dumps(metadata) + '\n')
    with ExitStack() as cleanup:
        errors = cleanup.enter_context((out / 'scanner.stderr').open('x'))
        log = cleanup.enter_context((out / 'boundaries.jsonl').open('x'))
        child = subprocess.Popen(list(map(str, command)),
                                 stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=errors, text=True,
                                 env={**os.environ, 'ZB_SCAN_THREADS': str(threads)})
        cleanup.enter_context(child)
        cleanup.callback(child.stdin.close)
        ready = exchange(child, None, 'ready')
        if ready.get('scan_threads', 1) != threads:
            raise RuntimeError('scanner thread count disagrees with requested setting')
        last_epoch = None
        for i, (phase, step, triggers, response) in enumerate(events):
            assert slots_now() == (slots, devices)
            event = dict(index=i, phase=phase, step=step, triggers=triggers, runtime=runtime, vm_id=api.vm_id,
                         vmm_pid=pid, vmm_start_ticks=int(start), hook_begin_ns=time.monotonic_ns())
            try:
                boundary(api, child, event, samples, slots, devices, snapshot, i > 0, dirty_slots, out, last_epoch)
                if dirty_slots:
                    last_epoch = event['dirty']['epoch']
                if phase == 'session-end':
                    event['original_snapshot_sample'] = sample_snapshot(snapshot, slots, len(samples))
                event['status'] = 'ok'
            except BaseException as exc:
                event.update(status='error', error=str(exc))
                raise
            finally:
                event['export_end_ns'] = time.monotonic_ns()
                event['hook_end_ns'] = time.monotonic_ns()
                response.update(event['freeze'])
                print(json.dumps(event, separators=(',', ':')), file=log, flush=True)
        child.stdin.close()
        if child.wait(timeout=20):
            raise RuntimeError(f'native deep scanner exit {child.returncode}')


def serve(run, threads):
    # Each adapter callback waits for a receipt before releasing the tool or LLM wait.
    out = run / 'native-deep'
    out.mkdir(mode=0o700)
    path = run / 'boundary.sock'
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(str(path))
        try:
            server.listen(1)
            identity = []
            def events():
                previous = None
                while previous != 'session-end':
                    conn, _ = server.accept()
                    with conn, conn.makefile('rb') as stream:
                        raw = stream.readline(8193)
                        if len(raw) > 8192 or not raw.endswith(b'\n'):
                            raise ValueError('incomplete native boundary callback')
                        phase, runtime, step, api_socket, triggers = json.loads(raw)
                        phases = {'session-start', 'session-end', 'pre-tool', 'post-tool', 'tool', 'parallel-tool', 'wait'}
                        if phase not in phases or (previous is None) != (phase == 'session-start'):
                            raise ValueError(f'unexpected callback {phase}')
                        if not triggers or set(triggers) - (phases | {'timer'}):
                            raise ValueError('unknown capture trigger')
                        if not identity:
                            identity.extend((runtime, api_socket))
                        if identity != [runtime, api_socket]:
                            raise RuntimeError('VMM changed between adapter callbacks')
                        response = {}
                        yield phase, step, triggers, response
                        conn.sendall((json.dumps(response) + '\n').encode())
                        previous = phase

            from itertools import chain
            with ExitStack() as cleanup:
                callbacks = events()
                cleanup.callback(callbacks.close)
                first = next(callbacks)
                collect(run, out, threads, chain([first], callbacks), *identity)
        finally:
            path.unlink(missing_ok=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('run', type=Path)
    args = parser.parse_args()
    if os.geteuid() != 0 or Path('/proc/self/ns/pid').stat().st_ino != INITIAL_PID_NAMESPACE:
        parser.error('native collector requires the initial host PID namespace and root')
    signal.signal(signal.SIGTERM, signal_error)
    signal.signal(signal.SIGINT, signal_error)
    serve(args.run.resolve(), 2)
