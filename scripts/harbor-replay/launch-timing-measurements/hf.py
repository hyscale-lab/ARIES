"""Repeat the same per-arm HF launch on its qualified single host CPU."""
from pathlib import Path
import os,json,subprocess,sys,time,shutil
p=Path(__file__).resolve().parent
v=sys.argv[1]
a=json.loads((p/('hf-'+v+'.json')).read_text())['argv']
target=p/('b615a3ec-'+v)
if target.exists():raise FileExistsError(target)
runtime=Path(a[a.index('-checkpoint-dir')+1]).parent
unit=Path(a[a.index('-socket')+1]).parent.name
cpu=subprocess.check_output(['systemctl','show',unit,'--property=EffectiveCPUs','--value'],text=True).strip()
if cpu!='15':raise ValueError(f'unexpected CPU affinity {cpu}')
if v=='v1':
    while subprocess.run(['systemctl','is-active','--quiet','zb-timing17-ordinary']).returncode==0:time.sleep(10)
env=dict(os.environ,ZB_VMM_CENSUS='1')
if v in ('v1','v2'):
    env['ZB_CHECKPOINT_NAMESPACE_DIR']=str(runtime/'checkpoints/aries')
if v=='v1':
    b='/users/nehalem/zepbound-demand-amd203/tools/bpf/usr'
    env.update(PATH=b+'/bin:'+env['PATH'],LD_LIBRARY_PATH=b+'/lib/x86_64-linux-gnu:'+b+'/lib/llvm-18/lib')
a[1]=p/'aries'
a[3]=target
args=[sys.executable,p/'vmm_exclusivity.py',str(target)+'-placement.jsonl','--',*a,'-timing-audit']
with target.with_suffix('.log').open('w') as log:
    rc=subprocess.run(args,env=env,stdout=log,stderr=log).returncode
target.mkdir(exist_ok=True)
(target/'driver.exit').write_text(str(rc)+'\n')
shutil.copy2(runtime/'runtime.json',target/'runtime.json')
