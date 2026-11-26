#!/usr/bin/env python3
"""Join planned startup reads to the first traced tool's observed guest page offsets."""
import argparse
import csv
import gzip
import hashlib
import json
from pathlib import Path
import sys

from pack import PAGE, pages

def rows(path):
    if not path.is_file():
        path=Path(str(path)+'.gz')
    opener=gzip.open if path.suffix=='.gz' else open
    with opener(path, 'rt') as f:
        return list(map(json.loads,f))

def load_bytes(path):
    if path.is_file():
        return path.read_bytes()
    with gzip.open(str(path)+'.gz','rb') as f:
        return f.read()

def inspect(run):
    lifecycle=rows(run/'lifecycle.jsonl')
    events=rows(run/'events.jsonl')
    deep,=run.rglob('boundaries.jsonl')
    captures=rows(deep)
    result=[]
    for restore in (v for v in lifecycle if v['event']=='restore_ready' and v['phase']=='restore'):
        snap=restore['snapshot_id']
        build,=[v for v in lifecycle if v['event']=='pack_built' and v['snapshot_id']==snap]
        read,=[v for v in lifecycle if v['event']=='prefetch' and v['snapshot_id']==snap]
        generation=restore['generation']
        directory=deep.parent/f'generation-{generation}'
        meta=json.loads((directory/'metadata.json').read_bytes())
        assert meta['pid']==restore['pid'] and meta['generation']==generation and meta['page_bytes']==PAGE
        assert meta['page_index']=='contiguous snapshot image offset divided by page_bytes'
        blob=(run/build['manifest']).read_bytes()
        assert hashlib.sha256(blob).hexdigest()==build['descriptor']['index_sha256']
        offsets={p//PAGE for p in pages(blob,build['budget_bytes'])}
        assert len(offsets)*PAGE==read['requested_bytes']
        step=restore['step']
        dispatch=min(e['monotonic_ns'] for e in events if e['event']=='tool_dispatch' and e['step_id']==step)
        picked=[v for v in captures if v['generation']==generation and v['step']==step and v['phase'] in ('tool','parallel-tool','post-tool') and v.get('window')]
        assert picked and picked[-1]['phase']=='post-tool'
        accessed=set(); valid_idle=None
        for v in picked:
            codes=load_bytes(directory/v['window']['file'])
            assert len(codes)*PAGE==meta['machine']['mem_size_mib']*(1<<20)
            used={i for i,b in enumerate(codes) if b&63==1}
            idle={p for p in offsets if codes[p]&63==2}
            valid_idle=idle if valid_idle is None else valid_idle&idle
            accessed.update(used)
        hits=offsets&accessed
        result.append(dict(snapshot_id=snap,step=step,generation=generation,build_to_dispatch_s=(dispatch-build['time_ns'])/1e9,
                      stock_restore_to_identity_s=(restore['time_ns']-next(v['begin_ns'] for v in lifecycle if v['event']=='restore_created' and v['snapshot_id']==snap))/1e9,
                      planned_bytes=read['requested_bytes'],completed_bytes=read['read_bytes'],observed_accessed_bytes=len(accessed)*PAGE,
                      selected_accessed_bytes=len(hits)*PAGE,selected_idle_all_tool_windows_bytes=len(valid_idle)*PAGE,
                      selected_unknown_bytes=(len(offsets)-len(hits)-len(valid_idle))*PAGE,unselected_observed_accessed_bytes=len(accessed-hits)*PAGE))
    return result
if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('run',type=Path)
    records=inspect(parser.parse_args().run)
    if records:
        writer=csv.DictWriter(sys.stdout,fieldnames=records[0],lineterminator='\n')
        writer.writeheader();writer.writerows(records)
