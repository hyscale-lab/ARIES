"""Publish replay receipts and compact per-page evidence for a completed TB4.1 session."""
import argparse
from collections import Counter
import gzip
import hashlib
from itertools import groupby
import json
from pathlib import Path
import shutil
import sys
from tempfile import TemporaryDirectory

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from guest_classes import cross_tab
from replay_windows import identify
from dirty import validate

PAGE = 4096


def rows(path):
    with path.open() as stream:
        for line in stream:
            if not line.endswith('\n'):
                raise ValueError(f'incomplete record in {path}')
            yield json.loads(line)


def write(stream, record):
    stream.write(json.dumps(record, separators=(',', ':')) + '\n')


def pack(output, name, data):
    (output / name).write_bytes(gzip.compress(data, compresslevel=4, mtime=0))
    return dict(file=name, sha256=hashlib.sha256(data).hexdigest(), bytes=len(data))


def parse_window(raw, output, row, slots, previous):
    index = row['index']
    data = (raw / f'idle-{index:02d}.u8').read_bytes()
    counts = Counter(data)
    reported = row['window']['code_counts']
    if (len(data) != sum(length for _, length in slots.values()) // PAGE
            or [sum(n for byte, n in counts.items() if byte & 63 == i) for i in range(len(reported))] != reported
            or sum(reported) != len(data)):
        raise ValueError('idle evidence does not match the capture counts')
    absent = []
    offset = 0
    for base, length in slots.values():
        cursor = base
        for is_absent, run in groupby(memoryview(data)[offset:offset+length//PAGE], key=lambda b: bool(b & 128)):
            end = cursor + sum(1 for _ in run) * PAGE
            if is_absent:
                absent.append([cursor,end])
            cursor = end
        offset += length // PAGE
    if len(data) - sum(b-a for a,b in absent)//PAGE != row['capture']['resident_pages_at_end']:
        raise ValueError('endpoint residency differs from captured idle bytes')
    classes, tagged = cross_tab(row, absent)
    slot_rows = []
    for slot in row['dirty']['slots']:
        bits = (raw / slot['bitmap_file']).read_bytes()
        ref = pack(output, f"dirty-{index:03d}-{slot['slot']}.gz", bits)
        if (ref['sha256'] != slot['bitmap_sha256'] or len(bits) != (slot['pages']+7)//8
                or int.from_bytes(bits, 'little').bit_count() != slot['dirty_pages']
                or slot['pages']%8 and bits[-1] >> (slot['pages']%8)):
            raise ValueError('invalid dirty bitmap')
        slot_rows.append({**slot,'bitmap_file':ref['file']})
    phase = 'tool' if previous['phase'] in ('pre-tool','tool','parallel-tool') else 'wait'
    return dict(index=index,phase=phase,step_id=previous['step'] if phase=='tool' else row['step'],
                begin_ns=previous['arm']['arm_end_ns'],end_ns=row['capture']['scan_begin_ns'],
                idle=pack(output,f'idle-{index:03d}.gz',data),code_counts=reported,
                kvm_dirty={**row['dirty'],'slots':slot_rows},classes=classes),tagged


def attribution(native, destination):
    with gzip.open(destination, 'xt', compresslevel=3) as stream:
        for r in rows(native/'boundaries.jsonl'):
            g=r['guest_telemetry']['guest']
            regular=g.get('regular_files')
            write(stream,dict(index=r['index'],probe_begin_ns=g['begin_monotonic_ns'],
                              probe_end_ns=g['end_monotonic_ns'],pfn_scan=g.get('pfn_scan'),
                              regular_files=None if regular is None else
                              {k:v for k,v in regular.items() if k!='files'}))


def process(run, output):
    events = list(rows(run/'events.jsonl'))
    if events[-1]['event'] != 'removed':
        raise ValueError('incomplete runtime cleanup')
    ending = [r for r in events if r['event']=='session_end_processes']
    handles = {}
    if ending:
        receipt, = ending
        handles = receipt['source_process_states']
        if any(state not in ('running','terminal_retained') for state in handles.values()):
            raise ValueError('missing source process at teardown')
    keys=('sequence','step_id','source_call_id','operation','source_process_session_id','exit_code','expected_exit_code')
    early={tuple(r.get(k) for k in keys) for r in events if r['event']=='diagnostic_process_observation'
           and r.get('status_basis')=='real_terminal_before_source_running_poll'
           and r.get('exit_code',-1)>=0 and r.get('expected_exit_code')==-1}
    divergent=[r for r in events if r['event']=='tool_reply' and r['exit_code']!=r['expected_exit_code']]
    for r in divergent:
        if r.get('operation') in ('StartProcess','PollProcess') and tuple(r.get(k) for k in keys) not in early:
            raise ValueError('asynchronous status divergence requires a matching process observation')
    groups, process_status = identify(events, complete=True, relaxed_status=bool(divergent),
                ending_handles=[int(h) for h,state in handles.items() if state=='running'],
                ending_retained_handles=[int(h) for h,state in handles.items() if state=='terminal_retained'])
    process_status['divergent_first_replies']=[{k:r.get(k) for k in keys} for r in divergent]
    create, = [r for r in events if r['event']=='create_start']
    lifecycle = [r['event'] for r in events]
    required = ['source_task_ready','snapshot_complete','restore_complete','session_start','tool_dispatch','session_end']
    if [lifecycle.index(k) for k in required] != sorted(lifecycle.index(k) for k in required):
        raise ValueError('invalid replay lifecycle')
    meta = dict(workload=create['task'],source_trial=create['trial_id'],job_id=create['job_id'],
                image=create['image'],trajectory_sha256=create['trajectory_sha256'],rollout_sha256=create['rollout_sha256'],
                process_status=process_status,group_count=len(groups))
    with (output/'events.jsonl').open('x') as stream:
        for event in events:
            write(stream,{k:v for k,v in event.items() if k not in {'stdout','stderr','values','content'}})
    for name in ('samples.jsonl','provenance.txt','effective-runtime.json'):
        if (run/name).is_file():
            shutil.copyfile(run/name, output/name)
    native=run/'native-deep'
    captures=[r for r in events if r['event']=='trace_capture']
    if native.exists():
        info=json.loads((native/'metadata.json').read_bytes())
        slots={s['slot']:(s['guest_phys_addr'],s['memory_size']) for s in info['ram_slots']}
        meta.update(ram_slots=info['ram_slots'],scan_threads=info['scan_threads'],codes=info['codes'],
                    vmm_sha256=info['vmm_sha256'],vmm_pid=info['vmm_pid'],start_ticks=info['vmm_start_ticks'])
        if info['scan_threads']!=2 or not info['kvm_dirty'] or info['runtime']!=create['runtime_id']:
            raise ValueError('wrong collector configuration')
        previous=None
        with (output/'windows.jsonl').open('x') as windows, gzip.open(output/'content.jsonl.gz','xt') as content:
            for i,(r,c) in enumerate(zip(rows(native/'boundaries.jsonl'),captures,strict=True)):
                if (r['index']!=i or r['phase']!=c['phase'] or r['step']!=c.get('step_id',0)
                        or r['triggers']!=c['triggers'] or r['status']!='ok' or r['vmm_pid']!=info['vmm_pid']
                        or r['vmm_start_ticks']!=info['vmm_start_ticks']):
                    raise ValueError('capture and harness disagree')
                validate(r['dirty'],PAGE,slots)
                if len(r['content_sample'])!=8192 or len({s['gfn'] for s in r['content_sample']})!=8192:
                    raise ValueError('incorrect content sample')
                r.update(ram_slots=info['ram_slots'],guest_ram_bytes=sum(length for _,length in slots.values()))
                tagged=r['content_sample']
                if previous:
                    if (r['dirty']['epoch']!=previous['dirty']['epoch']+1
                            or [s['gfn'] for s in tagged]!=[s['gfn'] for s in previous['content_sample']]):
                        raise ValueError('inconsistent window identity')
                    w,tagged=parse_window(native,output,r,slots,previous)
                    write(windows,w)
                else:
                    if r['phase']!='session-start':
                        raise ValueError('missing initial arm')
                    meta['dirty_reset']=r['dirty']
                write(content,dict(index=i,samples=tagged,checkpoint=r.get('original_snapshot_sample')))
                previous=r
        if previous is None or previous['phase']!='session-end' or len(previous['original_snapshot_sample'])!=8192:
            raise ValueError('missing final checkpoint comparison')
        attribution(native,output/'attribution.jsonl.gz')
        with gzip.open(output/'boundaries.jsonl.gz','xt') as stream:
            for r in rows(native/'boundaries.jsonl'):
                write(stream,{k:v for k,v in r.items() if k not in {'guest_telemetry','content_sample','original_snapshot_sample','dirty'}})
    elif any(c.get("pause_ack_monotonic_ns") or c.get("resume_request_monotonic_ns") for c in captures):
        raise ValueError('harness trace without native evidence')
    (output/'run.json').write_text(json.dumps(meta,indent=2)+'\n')


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('run',type=Path)
    p.add_argument('destination',type=Path)
    a=p.parse_args()
    if a.destination.exists():
        p.error('destination already exists')
    a.destination.parent.mkdir(parents=True,exist_ok=True)
    with TemporaryDirectory(dir=a.destination.parent) as tmp:
        stage=Path(tmp)/'run'
        stage.mkdir()
        process(a.run,stage)
        stage.rename(a.destination)
