"""Join snapshot discard ranges to host residency at the preceding idle arm."""
import argparse
import json
from pathlib import Path


def observations(path):
    return [json.loads(s) for s in path.read_text().splitlines()]


def join(raw, syscalls, metrics):
    captures = observations(raw / 'boundaries.jsonl')
    events = observations(raw / 'events.jsonl')
    start, = [e['monotonic_ns'] for e in events if e['event']=='session_start']
    end, = [e['monotonic_ns'] for e in events if e['event']=='session_end']
    pid = captures[0]['vmm_pid']
    slot, = captures[0]['ram_slots']
    base, size = slot['userspace_addr'], slot['memory_size']
    windows = [(c['window'], c['window_kind'], c['step']) for c in captures if 'window' in c]
    records = []
    for line in syscalls.read_text().splitlines():
        if not line.startswith('fixed '): continue
        t, owner, addr, n, ret = map(int,line.split()[1:])
        if owner!=pid or ret!=addr or not start<=t<=end or not base<=addr<addr+n<=base+size: continue
        lo,hi = addr-base,addr-base+n
        row = dict(t_ns=t-start,gpa=lo,bytes=n,absent_at_arm_bytes=None)
        for w, phase, step in windows:
            if w['arm_end_ns']<=t<=w['scan_begin_ns']:
                absent = sum(max(0,min(hi,b)-max(lo,a)) for a,b in w['not_resident_at_arm_gpa_ranges'])
                row.update(phase=phase,step=step,arm_age_s=(t-w['arm_end_ns'])/1e9,absent_at_arm_bytes=absent)
                break
        records.append(row)
    device=observations(metrics)
    assert all(x['pid']==pid for x in device)
    counters={k:sum(x['balloon'][k] for x in device) for k in ('free_page_report_count','free_page_report_freed','free_page_report_fails')}
    return dict(runtime=captures[0]['runtime'],pid=pid,start_ticks=captures[0]['vmm_start_ticks'],
                session_start_ns=start,session_end_ns=end,device=counters,records=records)


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    for k in ('raw','syscalls','metrics'):p.add_argument(k,type=Path)
    a=p.parse_args()
    print(json.dumps(join(a.raw,a.syscalls,a.metrics),indent=2))
