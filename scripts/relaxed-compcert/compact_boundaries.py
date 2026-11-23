"""Read a single completed compiler trace and export its exact freeze and page-count evidence."""
import hashlib
import json
import os
from pathlib import Path
import resource
import sys
import time

def point(b,previous):
    f=b['freeze']
    w=b.get('window'); cats=b['vmm_census']['categories']
    ram=sum(t.get('Pss_bytes',0) for k,t in cats.items() if k.startswith('guest_ram'))
    detail=dict(phase=b['phase'],step=b['step'],start=f['pause_ack_ns'],end=f['resume_request_ns'],vmm_pid=b['vmm_pid'],
                guest_ram_pss_bytes=ram,census_categories=cats,previous_phase=previous,other_kvm_slots=b.get('other_kvm_slots',[]))
    if w:
        detail.update(window_bytes_per_page=w['page_bytes'],accessed_pages=len(w['accessed_gpas']),idle_pages=len(w['idle_gpas']),
                      unknown_reasons=w['unknown_counts'],not_resident_at_arm_pages=sum((hi-lo)//w['page_bytes'] for lo,hi in w['not_resident_at_arm_gpa_ranges']),
                      not_resident_at_end_pages=sum((hi-lo)//w['page_bytes'] for lo,hi in w['not_resident_at_end_gpa_ranges']),
                      window_scan_ns=[w['arm_begin_ns'],w['arm_end_ns'],w['scan_begin_ns'],w['scan_end_ns']])
    return detail

def export(raw,throttle=True):
    assert (raw/'idle-after.json').is_file() and (raw/'driver.exit').is_file()
    path=raw/'memory/idle.jsonl'
    before=path.stat();started=time.monotonic_ns();cpu=time.process_time();h=hashlib.sha256();results=[];identity=None;previous=None
    with path.open('rb') as f:
        if hasattr(os,'POSIX_FADV_NOREUSE'):os.posix_fadvise(f.fileno(),0,0,os.POSIX_FADV_NOREUSE)
        for line in f:
            h.update(line);b=json.loads(line);assert b['status']=='ok' and b['vm_id']==b['runtime']
            current=(b['runtime'],b['vmm_pid'],str(b['vmm_start_ticks']),b['guest_ram_bytes'])
            if identity is None:identity=current
            assert identity==current and b['guest_ram_bytes']==4096<<20
            t=b['freeze'];assert t['pause_request_ns']<=t['pause_ack_ns']<=t['resume_request_ns']<=t['resume_ack_ns']
            results.append(point(b,previous));previous=[b['phase'],b['step']]
            if throttle:
                # Keep each read below 12 MiB/s and this single idle-priority collector below 0.2 CPU on average.
                elapsed=(time.monotonic_ns()-started)/1e9
                delay=max(f.tell()/(12<<20),5*(time.process_time()-cpu))-elapsed
                if delay>0:time.sleep(delay)
    end=path.stat();assert before.st_ino==end.st_ino and before.st_size==end.st_size and before.st_mtime_ns==end.st_mtime_ns
    usage=resource.getrusage(resource.RUSAGE_SELF)
    return dict(identity=identity,rows=results,raw_sha256=h.hexdigest(),raw_bytes=end.st_size,raw_mtime_ns=end.st_mtime_ns,
                begin_host_monotonic_ns=started,end_host_monotonic_ns=time.monotonic_ns(),process_cpu_s=time.process_time()-cpu,
                max_rss_kib=usage.ru_maxrss,input_blocks=usage.ru_inblock,scheduler=os.sched_getscheduler(0),nice=os.getpriority(os.PRIO_PROCESS,0))
if __name__=='__main__':
    p=Path(sys.argv[1]);assert p.parent.parent==Path('/local/zb-relaxed-compcert-01') and p.parent.name in ('v3','v4')
    print(json.dumps(export(p),separators=(',',':')))
