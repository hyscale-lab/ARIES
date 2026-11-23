#!/usr/bin/env python3
"""Run a sealed single-source cohort and publish its terminal status."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys

from cohort_pypi import GRPC_LOOPBACK_TRIAL, GRPC_ROOT_SHA


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument('runtime',type=Path)
    cli.add_argument('unit')
    cli.add_argument('variant',choices=['v1','v2','v3','v4'])
    cli.add_argument('cni',type=Path)
    a=cli.parse_args()
    asset=Path(__file__).resolve().parent
    subprocess.run(['sha256sum','--quiet','-c','SHA256SUMS'],cwd=asset,check=True)
    manifest=asset/'manifest.json'
    entry,=json.loads(manifest.read_bytes())
    is_hf = entry['task'] == 'hf-model-inference'
    if is_hf:
        import cohort_hf_cpu
        expected_trial,root_sha,cni_flag=cohort_hf_cpu.TRIAL,cohort_hf_cpu.ROOT_SHA,'--hf-cni'
        if subprocess.check_output(['systemctl','show','--property=EffectiveCPUs','--value',a.unit],text=True).strip()!=str(cohort_hf_cpu.HOST_CPU):
            raise ValueError('private runtime lacks the source one-core cpuset')
    else:
        expected_trial,root_sha,cni_flag=GRPC_LOOPBACK_TRIAL,GRPC_ROOT_SHA,'--pypi-cni'
    if entry['trial_id']!=expected_trial or entry['task'] not in ('kv-store-grpc','hf-model-inference'):
        raise ValueError('unexpected cohort source')
    profile=json.loads((a.runtime/'runtime.json').read_bytes())
    if (profile['smt_enabled'] is not False or profile['guest_root_mode']!='disk' or
        bool(profile.get('free_page_reporting'))!=(a.variant=='v2') or
        bool(profile.get('root_drive_use_pmem'))!=(a.variant=='v4') or
        bool((profile.get('guest_reclamation') or {}).get('Enabled'))!=(a.variant in ('v3','v4')) or
        digest(Path(profile['root_drive']))!=root_sha):
        raise ValueError('runtime feature or guest OS differs')
    if subprocess.run(['pgrep','-x','firecracker|[.]firecracker-re'],stdout=subprocess.DEVNULL).returncode!=1:
        raise RuntimeError('worker must be idle before this single-source run')
    driver=asset/'run-reviewed-cohort.py'
    run=asset/'runs'/(expected_trial[:8]+'-'+a.variant+'-cohort-01')
    status=asset/'runs'/('cohort-'+a.variant+'-status.jsonl')
    if run.exists() or status.exists():
        raise FileExistsError('use a fresh worker-side bundle directory for each attempted primary')
    (asset/'runs').mkdir(exist_ok=True)
    rc=subprocess.run([sys.executable,'-B',str(driver),str(a.runtime),a.unit,str(asset),a.variant,str(manifest),cni_flag,str(a.cni)]).returncode
    row=dict(trial=expected_trial,task=entry['task'],variant=a.variant,driver_exit=rc,
             manifest_sha256=digest(manifest),driver_sha256=digest(driver),guest_os_sha256=root_sha)
    with status.open('x') as out:
        out.write(json.dumps(row)+'\n')
    return rc


if __name__=='__main__':
    sys.exit(main())
