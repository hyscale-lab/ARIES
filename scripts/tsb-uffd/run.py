#!/usr/bin/env python3
"""Worker-only sequential cold-restore UFFD tool-step experiment."""
import argparse
import importlib.util
import gzip
import json
import os
from pathlib import Path
import subprocess
import sys
import time
from control import send

PAGES = Path(__file__).resolve().parents[1]/'tsb-pages'
sys.path.insert(0,str(PAGES))
spec=importlib.util.spec_from_file_location('tsb_pages_run',PAGES/'run.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
Driver=module.Driver


def store(path,obj):
    path.write_text(json.dumps(obj,indent=2)+'\n')


def summarize(path,step_id):
    counts={};sets={};rows=0;final=None
    opener = gzip.open(path, 'rt') if path.suffix == '.gz' else path.open()
    with opener as f:
        for line in f:
            r=json.loads(line);rows+=1
            if r['event']=='summary':final=r
            if r['event']!='pagefault':continue
            phase=r['phase'];counts[phase]=counts.get(phase,0)+1
            # Snapshot materialization can cover all guest RAM: count separately.
            if phase!='snapshot':sets.setdefault(phase,set()).add(r['snapshot_page'])
    if final is None or final['failed']:raise RuntimeError('Missing/failed UFFD final summary')
    envelope=set().union(*sets.values()) if sets else set()
    tool=sets.get('tool',set())
    return {'step_id':step_id,'page_size':4096,'identity':'snapshot_offset / 4096; not host PFN or GPA',
            'phase_fault_events':counts,'phase_unique_pages':{p:len(v) for p,v in sets.items()},
            'tool_only_first_fault_pages':sorted(tool),'restore_to_tool_completion_first_fault_pages':sorted(envelope),
            'handler_summary':final,'event_rows':rows,
            'semantics':'Tool-only omits pages populated before tool start; envelope includes restore/validation/VM activity; snapshot phase excluded.'}


def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--output',type=Path,required=True)
    ap.add_argument('--commands',type=Path,required=True)
    ap.add_argument('--timeout-seconds',type=int,default=900)
    a=ap.parse_args();os.umask(0o077)
    root=a.output.resolve();root.mkdir(parents=True,exist_ok=False)
    raw=json.loads(a.commands.read_text());steps=[]
    for i,item in enumerate(raw):
        if isinstance(item,list):item={'id':f'deterministic-{i+1}','operations':[{'argv':item}]}
        item=dict(item);item.setdefault('id',f'deterministic-{i+1}')
        if not item.get('operations'):raise ValueError('Step must contain operations')
        steps.append(item)
    store(root/'commands.json',steps)
    store(root/'manifest.json',{'mode':'checkpoint-uffd','resources':{'vcpu':2,'memory_mib':4096},'paid_requests':0,
        'steps':len(steps),'deadline_seconds':a.timeout_seconds,'startup_warmup':'seed boot/disk/background identity before first snapshot',
        'boundaries':'one cold restore and one fresh snapshot per complete step; multiple sequential operations allowed',
        'measurement':'first missing-page events at 4096-byte granularity; not separate read and write sets',
        'scope':'deterministic native sandbox pilot, not LLM-generated calls or benchmark score'})
    scripts=Path(__file__).resolve().parent
    socket_path=str(root/'uffd.sock');control_path=str(root/'control.sock')
    if len(socket_path.encode())>=108:raise ValueError('Unix socket path too long')
    driver=None;handler=None;handler_err=None;all_summaries=[]
    driver_log=(root/'driver-stderr.log').open('x')
    journal=(root/'operations.jsonl').open('x',buffering=1)
    started=time.monotonic()
    def call(request):
        journal.write(json.dumps({'event':'request','monotonic_ns':time.monotonic_ns(),'request':request})+'\n')
        response=driver.call(request)
        journal.write(json.dumps({'event':'response','monotonic_ns':time.monotonic_ns(),'response':response})+'\n')
        if response.get('error'):raise RuntimeError('Driver '+request['id']+': '+response['error'])
        if response.get('exit_code',0)!=0:raise RuntimeError('Nonzero tool exit '+request['id'])
        return response
    try:
        driver=Driver(['bin/tsb-pages-vm','-output',str(root/'vm'),'-idle-mode','checkpoint',
                       '-uffd-socket',socket_path,'-timeout',str(a.timeout_seconds)+'s'],driver_log)
        ready=driver.receive();store(root/'ready.json',ready)
        if ready.get('event')!='ready':raise RuntimeError('Driver not ready')
        marker='uffd-'+str(time.time_ns())
        seed=call({'id':'identity-seed','argv':['/bin/sh','-c',
            'echo '+marker+' >/tmp/uffd-state-marker; sleep 3600 >/dev/null 2>&1 & echo $! >/tmp/uffd-background; cat /proc/sys/kernel/random/boot_id; cat /tmp/uffd-state-marker; cat /tmp/uffd-background'], 'cwd':'/','timeout_seconds':30})
        expected=seed['stdout'];store(root/'identity-seed.json',seed)
        store(root/'initial-suspend.json',call({'id':'initial-suspend','op':'suspend'}))
        state_path=Path(ready['checkpoint_state'])
        for index,step in enumerate(steps):
            if time.monotonic()-started>a.timeout_seconds:raise TimeoutError('Pilot deadline reached')
            stepdir=root/f'step-{index+1:03d}';stepdir.mkdir();state=json.loads(state_path.read_text())
            store(stepdir/'before-state.json',state)
            if state['phase']!='suspended':raise RuntimeError('Expected suspended VM before cold restore')
            handler_err=(stepdir/'handler-stderr.log').open('x')
            handler=subprocess.Popen([sys.executable,str(scripts/'handler.py'),'--socket',socket_path,
                '--control',control_path,'--memory',state['memory'],'--log',str(stepdir/'faults.jsonl')],stderr=handler_err)
            deadline=time.monotonic()+10
            while not Path(control_path).exists():
                if handler.poll() is not None or time.monotonic()>deadline:raise RuntimeError('Handler failed before restore')
                time.sleep(.02)
            send(control_path,'status')
            store(stepdir/'resume.json',call({'id':step['id']+'-resume','op':'resume'}))
            send(control_path,'phase','restore_validation',step['id'])
            check=call({'id':step['id']+'-identity','argv':['/bin/sh','-c',
                'cat /proc/sys/kernel/random/boot_id; cat /tmp/uffd-state-marker; cat /tmp/uffd-background; kill -0 $(cat /tmp/uffd-background)'], 'cwd':'/','timeout_seconds':30})
            store(stepdir/'identity.json',check)
            if check['stdout']!=expected:raise RuntimeError('Boot/disk/background identity changed')
            start=send(control_path,'phase','tool',step['id']);store(stepdir/'tool-start.json',start)
            for n,operation in enumerate(step['operations']):
                request={'id':step['id']+'-op-'+str(n+1),'cwd':'/','timeout_seconds':180,**operation}
                store(stepdir/f'operation-{n+1:03d}.json',call(request))
            end=send(control_path,'phase','snapshot',step['id']);store(stepdir/'tool-end.json',end)
            store(stepdir/'suspend.json',call({'id':step['id']+'-suspend','op':'suspend'}))
            handler.wait(timeout=20)
            if handler.returncode!=0:raise RuntimeError('UFFD handler failed')
            handler=None;handler_err.close();handler_err=None
            result=summarize(stepdir/'faults.jsonl',step['id']);store(stepdir/'summary.json',result)
            all_summaries.append(result)
            print(step['id'],'tool first faults',len(result['tool_only_first_fault_pages']),
                  'restore envelope',len(result['restore_to_tool_completion_first_fault_pages']),
                  'snapshot events',result['phase_fault_events'].get('snapshot',0),flush=True)
        comparisons=[]
        for before,after in zip(all_summaries,all_summaries[1:]):
            comparison={'before':before['step_id'],'after':after['step_id']}
            for field in ['tool_only_first_fault_pages','restore_to_tool_completion_first_fault_pages']:
                left,right=set(before[field]),set(after[field]);union=left|right
                comparison[field]={'before_pages':len(left),'after_pages':len(right),'intersection':len(left&right),
                    'union':len(union),'jaccard':len(left&right)/len(union) if union else None,'added':len(right-left),'removed':len(left-right)}
            comparisons.append(comparison)
        store(root/'comparison.json',comparisons)
        store(root/'result.json',{'status':'passed','steps_completed':len(all_summaries),'boot_disk_background_identity_preserved':True,
            'elapsed_seconds':time.monotonic()-started,'scope':'actual VM deterministic UFFD checkpoint pilot; not LLM calls; first-touch envelope semantics'})
    except BaseException as exc:
        store(root/'failure.json',{'type':type(exc).__name__,'message':str(exc),'elapsed_seconds':time.monotonic()-started})
        raise
    finally:
        # Keep fault handler serving while owned VM cleanup runs.
        if driver:
            code=driver.close();store(root/'driver-exit.json',{'exit_code':code})
        if handler is not None:
            try:handler.wait(timeout=20)
            except subprocess.TimeoutExpired:
                # Driver cleanup already attempted; retain exact helper termination.
                handler.kill();handler.wait(timeout=10)
            store(root/'handler-exit.json',{'exit_code':handler.returncode})
        if handler_err:handler_err.close()
        journal.close();driver_log.close()


if __name__=='__main__':main()
