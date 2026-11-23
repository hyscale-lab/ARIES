#!/usr/bin/env python3
"""Collect same-VM guest and host evidence around the fixed treated/sham control."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time

POINTS={('session-start',0),('post-tool',10),('post-tool',12),('pre-tool',14),('post-tool',14),('post-tool',15),('session-end',0)}
PAGE=4096
now=time.monotonic_ns

def log(path, value):
    with path.open('a') as stream:
        print(json.dumps(value,separators=(',',':')),file=stream)

def start_ticks(pid):
    return int(Path(f'/proc/{pid}/stat').read_text().rsplit(')',1)[1].split()[19])

def present(stage,out,name,pid,ticks,slots):
    begin=now()
    assert start_ticks(pid)==ticks
    layout=[]
    packed=bytearray()
    for slot in sorted(slots,key=lambda r:r['guest_phys_addr']):
        gpa,size,addr=(slot[k] for k in ('guest_phys_addr','memory_size','userspace_addr'))
        pages=size//PAGE
        assert size%PAGE==0 and pages%8==0 and gpa%PAGE==0 and addr%PAGE==0
        lo=now()
        data=subprocess.check_output([stage/'present-bits',str(pid),str(addr),str(pages)])
        hi=now()
        assert len(data)==pages//8
        layout.append(dict(slot=slot['slot'],gpa=gpa,hva=addr,pages=pages,byte_offset=len(packed),begin_ns=lo,end_ns=hi))
        packed.extend(data)
    end=now()
    assert start_ticks(pid)==ticks and len(packed)==(8<<30)//(PAGE*8)
    filename=f'{name}.bits'
    (out/'presence'/filename).write_bytes(packed)
    return dict(file=filename,sha256=hashlib.sha256(packed).hexdigest(),begin_ns=begin,end_ns=end,
                layout=layout,present_pages=int.from_bytes(packed,'little').bit_count())

def main():
    phase,runtime,step,socket=sys.argv[1:]
    step=int(step); begin=now()
    out=Path(os.environ['ZB_PAIR_OUT']);stage=Path(os.environ['ZB_PAIR_STAGE']);arm=os.environ['ZB_PAIR_ARM']
    assert arm in ('treated','sham')
    sys.path.insert(0,os.environ['ZB_CACHE_TRACE_ASSETS'])
    from idle_hook import Firecracker,select_slots
    from vmm_census import census
    from guest_sample import snapshot
    api=Firecracker(socket);api.inspect();assert api.vm_id==runtime
    pid,ticks=api.identity
    ram,other=select_slots(Path(os.environ['ZB_SLOT_EVENTS']).read_text(),pid,ticks,1_000_000_000//os.sysconf('SC_CLK_TCK'),8<<30)
    assert not other
    def ram_pss():
        lo=now(); c=census(pid,ram,other);hi=now()
        assert 'mixed_vma' not in c['categories']
        return dict(begin_ns=lo,end_ns=hi,pss_bytes=c['categories']['guest_ram']['Pss_bytes'],census=c,ram_slots=ram)
    row=dict(phase=phase,step=step,runtime=runtime,vmm_pid=pid,vmm_start_ticks=ticks,host_observer_begin_ns=begin,arm=arm)
    vsock=api.path.with_name('firecracker.vsock')
    if (phase,step) in POINTS:
        q=snapshot(vsock)
        assert q['guest']['torch_file_cache']['task_root']==f'/container/{runtime}/rootfs'
        row.update(q)
    row['guest_ram_before']=ram_pss()
    if (phase,step)==('post-tool',12):
        assert Path(os.environ['ZB_RELEASE_HANDLER_EVENTS']).read_text().startswith('ready ')
        row['presence_before']=present(stage,out,'post12-before',pid,ticks,ram)
        row['wheel_release']=snapshot(vsock,request=b'evictwhl\n' if arm=='treated' else b'shamwhls\n')
        result=row['wheel_release']['guest']
        assert not result.get('error') and result['advice_syscalls_enabled']==(arm=='treated')
        row['presence_after']=present(stage,out,'post12-after',pid,ticks,ram)
        row['guest_ram_after']=ram_pss()
    elif (phase,step) in POINTS:
        row['presence']=present(stage,out,f'{phase}-{step}',pid,ticks,ram)
    row['host_observer_end_ns']=now()
    log(out/'boundaries.jsonl',row)

if __name__=='__main__':main()
