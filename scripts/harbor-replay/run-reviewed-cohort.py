#!/usr/bin/env python3
"""Run a fixed cohort of previously validated Harbor recordings."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

from cohort_image_circuit import IMAGE_HELPER, TASKS, digest as file_digest, verify


PYPI_TASKS = {'kv-store-grpc', 'financial-document-processor', 'hf-model-inference', 'dna-assembly'}
SOURCE_DEPENDENCY_TASKS = {'sparql-university', 'torch-tensor-parallelism'}


def main():
    cli=argparse.ArgumentParser(description=__doc__)
    cli.add_argument('runtime',type=Path);cli.add_argument('unit')
    cli.add_argument('assets',type=Path);cli.add_argument('variant',choices=('v1','v2','v3','v4'))
    cli.add_argument('manifest',type=Path)
    cli.add_argument('--sparql-cni',type=Path,help='private pre-provisioned SPARQL network profile')
    cli.add_argument('--torch-cni',type=Path,help='private pre-provisioned CPU Torch network profile')
    cli.add_argument('--video-cni',type=Path,help='private video dependency network profile')
    cli.add_argument('--dataset-cni',type=Path,help='private token dataset network profile')
    cli.add_argument('--pypi-cni',type=Path,help='private gRPC and financial PyPI network profile')
    cli.add_argument('--financeapt-cni',type=Path,help='private signed-snapshot financial network profile')
    cli.add_argument('--marker-cni',type=Path,help='private signed archive network for the marker fixture')
    cli.add_argument('--hf-cni',type=Path,help='private inference model and dependency network profile')
    a=cli.parse_args()
    if not re.fullmatch(r'fcmod-zepbound-[a-z0-9-]+',a.unit):
        raise ValueError('unexpected unit')
    work=a.runtime.resolve(strict=True);asset=a.assets.resolve(strict=True)
    socket='/run/'+a.unit+'/containerd.sock'
    env=dict(os.environ,ZB_VMM_CENSUS='1')
    base=['-namespace','aries','-socket',socket,'-checkpoint-dir',str(work/'checkpoints'),
          '-shim-dir',str(work/'shim'),'-io-dir','/run/'+a.unit+'/aries-io']
    if a.variant in ('v3','v4'):
        env.pop('ZB_CHECKPOINT_NAMESPACE_DIR',None)
        base.append('-native-checkpoint')
    else:
        env['ZB_CHECKPOINT_NAMESPACE_DIR']=str(work/'checkpoints/aries')
    entries=json.loads(a.manifest.read_text())
    for entry in entries:
        task=entry['task'];trial=entry['trial_id']
        if task not in {'constraints-scheduling','regex-log','overfull-hbox','count-dataset-tokens','video-processing',*TASKS,*PYPI_TASKS,*SOURCE_DEPENDENCY_TASKS} or not re.fullmatch(r'[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}',trial):
            raise ValueError('unexpected trial')
        if task == 'count-dataset-tokens':
            import cohort_dataset
        if task == 'video-processing':
            import cohort_video
        if task in SOURCE_DEPENDENCY_TASKS:
            import cohort_sparql
            cni=a.sparql_cni if task == 'sparql-university' else a.torch_cni
        if task in PYPI_TASKS:
            import cohort_pypi
            receipt=cohort_pypi.load_receipt(asset,entry)
            financial_apt=entry.get('financial_apt')
            hf=entry.get('hf')
            if task == 'hf-model-inference':
                import cohort_hf
                if hf is None:
                    raise ValueError('inference trial is missing its source-bound artifact inventory')
                pypi_args=cohort_hf.replay_args(asset,a.hf_cni)
            elif task == cohort_pypi.MARKER:
                pypi_args=cohort_pypi.replay_args(asset,task,a.marker_cni,'marker')
            elif financial_apt is not None:
                pypi_args=cohort_pypi.replay_args(asset,task,a.financeapt_cni,'financeapt')
            else:
                pypi_args=cohort_pypi.replay_args(asset,task,a.pypi_cni)
            pypi_network=pypi_args[pypi_args.index('-cni-network')+1]
        else:
            receipt=entry['receipt']
        if receipt.get('diagnostic_process_drift') or receipt.get('diagnostic_video_images'):
            raise ValueError('diagnostic replay cannot qualify for a primary measurement')
        if not (receipt['session_completed'] and receipt['all_observed_tool_exits_match'] and receipt['trial_id']==trial and receipt['task']==task):
            raise ValueError('missing successful source-specific fidelity evidence')
        folder=asset/'sources'/task/trial
        direct=['-trajectory',str(folder/'trajectory.json'),'-rollout',str(folder/'rollout.jsonl')]
        digest=receipt['adapter_binary_sha256']
        if not re.fullmatch(r'[0-9a-f]{64}',digest):
            raise ValueError(f'invalid adapter digest for {trial}')
        cmd=asset/'adapters'/digest
        if not cmd.exists():
            cmd=asset/('aries-tex' if task=='overfull-hbox' else 'aries-multitask')
        if hashlib.sha256(cmd.read_bytes()).hexdigest()!=receipt['adapter_binary_sha256']:
            raise ValueError(f'adapter differs from validation receipt for {trial}')
        # Adapter enforces both source digests, image identity and resource contract.
        key=trial[:8]+'-'+a.variant+'-cohort'
        first=asset/'runs'/(key+'-01')
        if first.exists():
            events=first/'events.jsonl'
            if events.exists():
                try: done=json.loads(events.read_text().splitlines()[-1]).get('event')=='removed'
                except (ValueError,IndexError): done=False
                if done and (first/'driver.exit').is_file() and (first/'driver.exit').read_text().strip()=='0':
                    if task in TASKS:
                        verify(first,receipt)
                    if task == 'count-dataset-tokens':
                        cohort_dataset.verify(first, receipt)
                    if task == 'video-processing':
                        cohort_video.verify(first, receipt)
                    if task in SOURCE_DEPENDENCY_TASKS:
                        cohort_sparql.verify(events,receipt)
                    if task in PYPI_TASKS:
                        cohort_pypi.verify(events,receipt,entry['reference_boundaries'],pypi_network,financial_apt,hf)
                    continue
            raise FileExistsError(f'inspect incomplete attempt before retry: {first}')
        args=[str(asset/'tracer/run-traced.sh'),str(cmd),str(asset/'tracer'),str(first),*direct,*base,
              '-image',receipt['image'],'-codex-tools',str(asset/'codex-harbor-tools.tar.gz')]
        if task == 'code-from-image':
            helper = asset/'aries-harbor-image'
            if file_digest(helper) != IMAGE_HELPER:
                raise ValueError('source-validated guest image helper differs')
            args.extend(['-image-helper',str(helper)])
        elif task == 'circuit-fibsqrt':
            ready, = [e['state'] for e in receipt['source_setup_and_final_state'] if e['event']=='source_task_ready']
            if sha := ready.get('debian_patch_archive_sha256'):
                archive=asset/'circuit-patch-debs.tar.gz'
                if file_digest(archive)!=sha:
                    raise ValueError('Debian patch archive differs from the native validation')
                args.extend(['-os-tools',str(archive)])
        elif task == 'count-dataset-tokens':
            args.extend(cohort_dataset.replay_args(asset,receipt,a.dataset_cni))
        elif task == 'video-processing':
            args.extend(cohort_video.replay_args(asset,receipt,a.video_cni))
        elif task in SOURCE_DEPENDENCY_TASKS:
            args.extend(cohort_sparql.replay_args(asset,cni,task))
        elif task in PYPI_TASKS:
            args.extend(pypi_args)
        elif task not in TASKS:
            args.extend(['-os-tools',str(asset/(task+'-debs.tar.gz'))])
        rc=subprocess.run(args,env=env).returncode
        status=first/'driver.exit'
        if rc:
            status.write_text(str(rc)+'\n')
            raise RuntimeError(f'{trial} exited {rc}')
        try:
            checks=None
            if task in TASKS:
                checks=verify(first,receipt)
            elif task == 'count-dataset-tokens':
                checks=cohort_dataset.verify(first,receipt)
            elif task == 'video-processing':
                checks=cohort_video.verify(first,receipt)
            elif task in SOURCE_DEPENDENCY_TASKS:
                checks=cohort_sparql.verify(first/'events.jsonl',receipt)
            elif task in PYPI_TASKS:
                checks=cohort_pypi.verify(first/'events.jsonl',receipt,entry['reference_boundaries'],pypi_network,financial_apt,hf)
            if checks is not None:
                (first/'fidelity-checks.json').write_text(json.dumps(checks,indent=2)+'\n')
        except Exception:
            status.write_text('1\n')
            raise
        status.write_text('0\n')


if __name__=='__main__':
    main()
