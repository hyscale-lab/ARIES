"""Replay the measured early installer exit on the same source worker with native process observations."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

stage = Path(__file__).resolve().parent
initial = Path('/tmp/zb-marker-rest-01')
trial = 'ba8d88f7-b8ea-4a81-a197-a10e8126f9f8'
variant = sys.argv[1] if len(sys.argv) > 1 else 'v1'
worker, service, runtime, bundle, event_sha = {
    'v1': ('amd203','fcmod-zepbound-demand-amd203','/users/nehalem/zepbound-demand-amd203/runtime',
           '/local/zb-corpus-pypi-cohort-v1','f62a879be117050910e1f3f7c0573129418c427105f5525d68e54b8ef28cc4f6'),
    'v2': ('amd200','fcmod-zepbound-amd200','/local/zb-v2/runtime',
           '/local/zb-corpus-financeapt-cohort-v2','22d3f51438d0a8dddee6cd8d4eaf7f5d09faedf3c4e62a4b6cb3c2d8c7a3a20d'),
}[variant]
runtime, bundle = Path(runtime), Path(bundle)
previous = initial/f'ba8d88f7-{variant}'
trace = previous/'events.jsonl'
assert (previous/'driver.exit').read_text().strip()=='1'
assert hashlib.sha256(trace.read_bytes()).hexdigest()==event_sha
events = [json.loads(l) for l in trace.open()]
first, = [e for e in events if e['event']=='tool_reply' and e['step_id']==7]
assert (first['operation'],str(first['source_process_session_id']),first['exit_code'],first['expected_exit_code'])==('StartProcess','57006',0,-1)
assert first['stdout_sha256'] == first['source_output_sha256']
for unit in ('zb-marker-rest-01','zb-ocr-v2-recovery','zb-sanitize-retry-02'):
    while subprocess.run(['systemctl','is-active','--quiet',unit]).returncode==0:
        time.sleep(15)
source = initial/'dna-assembly'/trial
out = stage/previous.name
if out.exists():
    raise FileExistsError(out)
while subprocess.run(['pgrep','-x','firecracker|.firecracker-re'],stdout=subprocess.DEVNULL).returncode==0:
    time.sleep(15)
bpf = '/users/nehalem/zepbound-demand-amd203/tools/bpf/usr'
env = dict(os.environ,ZB_VMM_CENSUS='1',ZB_CHECKPOINT_NAMESPACE_DIR=str(runtime/'checkpoints/aries'))
if variant == 'v1':
    env.update(PATH=bpf+'/bin:'+os.environ['PATH'],LD_LIBRARY_PATH=bpf+'/lib/x86_64-linux-gnu')
tracer = bundle/'tracer'
net = initial/'net'
cmd = [sys.executable, initial/'vmm_exclusivity.py',stage/'placement.jsonl','--',tracer/'run-traced.sh',
       stage/'aries',tracer,out,'-diagnostic-process-drift','-trajectory',source/'trajectory.json','-rollout',source/'rollout.jsonl',
       '-namespace','aries','-socket','/run/'+service+'/containerd.sock','-checkpoint-dir',runtime/'checkpoints',
       '-shim-dir',runtime/'shim','-io-dir','/run/'+service+'/aries-io','-image',next(e['image'] for e in events if e['event']=='create_start'),
       '-codex-tools',bundle/'codex-harbor-tools.tar.gz','-os-tools',initial/'dna-assembly-debs.tar.gz',
       '-cni-network','zbmarksrc'+worker[3:],'-cni-conf-dir',net/'conf','-cni-cache-dir',net/'cache','-cni-bin-dir',net/'bin']
with (stage/'driver.log').open('x') as log:
    rc = subprocess.run(cmd,env=env,stdout=log,stderr=log).returncode
out.mkdir(exist_ok=True)
shutil.copy2(runtime/'runtime.json',out/'runtime.json')
(out/'driver.exit').write_text(str(rc)+'\n')
sys.exit(rc)
