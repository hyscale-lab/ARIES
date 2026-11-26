"""Collect terminal campaign attempts and reduce successful sessions on the runner."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import tarfile
from tempfile import TemporaryDirectory

from process import process


def collect(host, cache, results, stage='/local/zb-tb41-cohort/runs'):
    if not re.fullmatch(r'/local/(?:[a-zA-Z0-9_-]+/)+runs',stage):
        raise ValueError('expected a worker study runs directory under /local')
    command=['ssh','-o','BatchMode=yes',host]
    names=subprocess.check_output([*command, f"sudo find {stage} -maxdepth 1 -name '*.exit' -printf '%f\\n'"],text=True)
    for name in sorted(names.splitlines()):
        if not re.fullmatch(r'[a-z0-9-]+-campaign-[0-9]+\.exit',name):
            raise ValueError('unexpected worker attempt name')
        run=name[:-5]
        if (results/'runs'/run/'run.json').is_file() or (results/'attempts'/run/'attempt.json').is_file():
            continue
        dest=cache/run
        if not dest.exists():
            cache.mkdir(parents=True,exist_ok=True)
            with TemporaryDirectory(dir=cache) as tmp:
                path=Path(tmp)
                with subprocess.Popen([*command,f'sudo nice -n 19 ionice -c 3 tar czf - -C {stage} {run} {name} {run}.driver'],stdout=subprocess.PIPE) as proc:
                    with tarfile.open(fileobj=proc.stdout,mode='r|gz') as archive:
                        for info in archive:
                            if not (info.isfile() or info.isdir()):
                                raise ValueError(f'unexpected archive entry {info.name}')
                            archive.extract(info,path,filter='data')
                    if proc.wait():
                        raise ValueError(f'transfer failed for {run}')
                (path/name).rename(path/run/'driver.exit')
                (path/(run+'.driver')).rename(path/run/'driver.log')
                (path/run).rename(dest)
        status=int((dest/'driver.exit').read_text())
        target=results/('attempts' if status else 'runs')/run
        if target.exists():
            continue
        target.parent.mkdir(parents=True,exist_ok=True)
        with TemporaryDirectory(dir=target.parent) as tmp:
            draft=Path(tmp)/'output'
            draft.mkdir()
            if status:
                lines=(dest/'driver.log').read_text().strip().splitlines()
                evidence=dict(attempt=run,node=host.split('@')[-1].split('.')[0],exit_status=status,
                              driver_last_line=lines[-1] if lines else None)
                if (dest/'events.jsonl').exists():
                    with (dest/'events.jsonl').open('rb') as stream:
                        events=[json.loads(line) for line in stream if line.strip()]
                    evidence['source_trial']=events[0]['trial_id'] if events else None
                    evidence['first_replies']=[{k:e.get(k) for k in ('step_id','sequence','source_call_id','exit_code','expected_exit_code')} for e in events if e['event']=='tool_reply']
                (draft/'attempt.json').write_text(json.dumps(evidence,indent=2)+'\n')
            else:
                process(dest,draft)
                metadata=draft/'run.json'
                value=json.loads(metadata.read_bytes())
                value['node']=host.split('@')[-1].split('.')[0]
                metadata.write_text(json.dumps(value,indent=2)+'\n')
            draft.rename(target)
        print(run,status,flush=True)


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('host')
    p.add_argument('cache',type=Path)
    p.add_argument('results',type=Path)
    p.add_argument('--stage',default='/local/zb-tb41-cohort/runs')
    a=p.parse_args()
    collect(a.host,a.cache,a.results,a.stage)
