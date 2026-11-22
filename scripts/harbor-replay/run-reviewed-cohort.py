#!/usr/bin/env python3
"""Run a fixed cohort of previously validated regex, calendar and TeX recordings."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess


def main():
    cli=argparse.ArgumentParser(description=__doc__)
    cli.add_argument('runtime',type=Path);cli.add_argument('unit')
    cli.add_argument('assets',type=Path);cli.add_argument('variant',choices=('v1','v2','v3','v4'))
    cli.add_argument('manifest',type=Path)
    a=cli.parse_args()
    if not re.fullmatch(r'fcmod-zepbound-[a-z0-9-]+',a.unit):
        raise ValueError('unexpected unit')
    work=a.runtime.resolve(strict=True);asset=a.assets.resolve(strict=True)
    socket='/run/'+a.unit+'/containerd.sock'
    env=dict(os.environ,ZB_CHECKPOINT_NAMESPACE_DIR=str(work/'checkpoints/aries'),ZB_VMM_CENSUS='1')
    base=['-namespace','aries','-socket',socket,'-checkpoint-dir',str(work/'checkpoints'),
          '-shim-dir',str(work/'shim'),'-io-dir','/run/'+a.unit+'/aries-io']
    if a.variant in ('v3','v4'):base.append('-native-checkpoint')
    entries=json.loads(a.manifest.read_text())
    for entry in entries:
        task=entry['task'];trial=entry['trial_id']
        if task not in ('constraints-scheduling','regex-log','overfull-hbox') or not re.fullmatch(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}',trial):
            raise ValueError('unexpected trial')
        receipt=entry['receipt']
        if not (receipt['session_completed'] and receipt['all_observed_tool_exits_match'] and receipt['trial_id']==trial and receipt['task']==task):
            raise ValueError('missing successful source-specific fidelity evidence')
        folder=asset/'sources'/task/trial
        direct=['-trajectory',str(folder/'trajectory.json'),'-rollout',str(folder/'rollout.jsonl')]
        cmd=asset/'aries-tex'
        # Adapter enforces both source digests, image identity and resource contract.
        key=trial[:8]+'-'+a.variant+'-cohort'
        first=asset/'runs'/(key+'-01')
        if first.exists():
            events=first/'events.jsonl'
            if events.exists():
                try: done=json.loads(events.read_text().splitlines()[-1]).get('event')=='removed'
                except (ValueError,IndexError): done=False
                if done and (first/'driver.exit').read_text().strip()=='0':continue
            raise FileExistsError(f'inspect incomplete attempt before retry: {first}')
        args=[str(asset/'tracer/run-traced.sh'),str(cmd),str(asset/'tracer'),str(first),*direct,*base,
              '-image',receipt['image'],'-codex-tools',str(asset/'codex-harbor-tools.tar.gz'),
              '-os-tools',str(asset/(task+'-debs.tar.gz'))]
        rc=subprocess.run(args,env=env).returncode
        (first/'driver.exit').write_text(str(rc)+'\n')
        if rc:raise RuntimeError(f'{trial} exited {rc}')


if __name__=='__main__':
    main()
