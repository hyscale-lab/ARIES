"""Replay the single source under the dedicated guest filemap observer."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

def sha(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()

stage, assets = map(Path, sys.argv[1:3])
unit = sys.argv[3]
assert unit == 'fcmod-zepbound-wheel-pair-v3'
root = stage.parent.parent
arm = os.environ['ZB_PAIR_ARM']
assert arm in ('treated','sham')
runtime = root/'runtime'
cfg = json.loads((runtime/'runtime.json').read_bytes())
assert cfg['guest_reclamation'] == {'Enabled':True} and not cfg['root_drive_use_pmem'] and not cfg['smt_enabled']
assert sha(assets/'SHA256SUMS') == '6f50c5bd8fa791b90e600d487842668baf38e1e1835d68c3cc55426fa9dd06f2'
subprocess.run(['sha256sum','--quiet','-c','SHA256SUMS'],cwd=assets,check=True)
entry, = json.loads((assets/'manifest.json').read_bytes())
trial = '4b588047-22af-4dc2-a705-6b8d24ff88fe'
task = 'torch-tensor-parallelism'
src = assets/'sources'/task/trial
receipt = entry['receipt']
assert (entry['task'],entry['trial_id'],receipt['vcpus'],receipt['memory_mib'],receipt['storage_mib']) == (task,trial,1,8192,10240)
adapter = root/'clock-adapter'
assert sha(adapter) == '557158ab56d79ee3f6fca0300f6b0dc423724d4b795adecb5d738a6cb5155541'
sys.path.insert(0,str(assets))
from cohort_sparql import replay_args, verify
env = dict(os.environ,ZB_PAIR_STAGE=str(root),ZB_PAIR_OUT=str(stage), ZB_CACHE_TRACE_ASSETS=str(assets/'tracer'))
assert Path(env['ZB_RELEASE_TRANSPORT']).is_file()
args=[adapter,'-log',stage/'events.jsonl','-observer',root/'observer.py',
      '-trajectory',src/'trajectory.json','-rollout',src/'rollout.jsonl','-image',receipt['image'],
      '-namespace','aries','-socket',f'/run/{unit}/containerd.sock','-checkpoint-dir',runtime/'checkpoints',
      '-shim-dir',runtime/'shim','-io-dir',f'/run/{unit}/aries-io','-codex-tools',assets/'codex-harbor-tools.tar.gz',
      '-native-checkpoint','-guest-task-v3',*replay_args(assets,assets/'cni',task),
      '-sampler',assets/'tracer/sample_vmm.py','-samples',stage/'samples.jsonl']
rc=subprocess.run(args,env=env).returncode
if rc: sys.exit(rc)
checks=verify(stage/'events.jsonl',receipt)
(stage/'fidelity-checks.json').write_text(json.dumps(checks,indent=2)+'\n')
