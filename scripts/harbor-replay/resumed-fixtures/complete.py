"""Fill missing fixture endpoint retries and intermediate ordinary measurements."""
from pathlib import Path
import os,subprocess,sys,time,shutil
stage=Path(__file__).resolve().parent
node=int(sys.argv[1])
before='zb-timing17-hf' if node==203 else 'zb-timing17-ordinary'
while subprocess.run(['systemctl','is-active','--quiet',before]).returncode==0:time.sleep(10)
extra=Path('/tmp/zb-resume-extra-01')
if node==203:
    modes=['v1','deep-v1']
    bundle=Path('/local/zb-corpus-pypi-cohort-v1')
elif node==191:
    modes=['v4','deep-v4','v3']
    bundle=Path('/local/zb-fc-v4/pilot/pypi-cohort-amd191')
else:
    modes=['v2']
    bundle=Path('/local/zb-corpus-financeapt-cohort-v2')
def passed(folder):
    status=folder/'driver.exit'
    return status.exists() and status.read_text().strip()=='0'
for mode in modes:
    variant=mode.split('-')[-1]
    tracer=bundle/'tracer'
    unit,runtime={
        'v1':('fcmod-zepbound-demand-amd203','/users/nehalem/zepbound-demand-amd203/runtime'),
        'v2':('fcmod-zepbound-amd200','/local/zb-v2/runtime'),
        'v3':('fcmod-zepbound-amd191-v3','/local/zb-fc-v4/v3/runtime'),
        'v4':('fcmod-zepbound-amd191-v4','/local/zb-fc-v4/v4/runtime'),
        'deep-v1':('fcmod-zepbound-deep203cf','/local/zb-deep-cf-v1-amd203/runtime'),
        'deep-v4':('fcmod-zepbound-amd191-expdeep','/local/zb-expdeep-runtime-amd191/runtime'),
    }[mode]
    runtime=Path(runtime)
    env=dict(os.environ,ZB_VMM_CENSUS='1',ZB_CHECKPOINT_NAMESPACE_DIR=str(runtime/'checkpoints/aries'))
    if node==203:
        b='/users/nehalem/zepbound-demand-amd203/tools/bpf/usr'
        env.update(PATH=b+'/bin:'+env['PATH'],LD_LIBRARY_PATH=b+'/lib/x86_64-linux-gnu')
    if mode.startswith('deep'):
        env.update(ZB_GUEST_VSOCK='1',ZB_CONTENT_SAMPLE='8192')
        tracer=Path('/local/zb-deep-hf-v1-amd203/trace' if node==203 else '/local/zb-grpc69ba-v4/cohort/tracer')
    sock='/run/'+unit+'/containerd.sock'
    for task,digest in [('git-leak-recovery','62160e522a4238b8bcd522411ea97b5b200f811e5899d34947e66809bef7e586'),('dna-assembly','d1adf6835f1dd91205ba70e452c699d0aea601010038e5617f370716efb50569')]:
        if task=='dna-assembly' and variant in ('v1','v4'):continue
        image='docker.io/alexgshaw/'+task+'@sha256:'+digest
        netargs=[]
        if task=='dna-assembly':
            net=extra/'marker-cni'
            if node==200:
                net=stage/'net';net.mkdir(exist_ok=True)
                for name in ['SHA256SUMS','bin']:subprocess.run(['cp','-an',bundle/'cni'/name,net],check=True)
                subprocess.run(['bash',stage/'provision-sparql-cni.sh',net,'install','amd200','marker'],check=True)
            netargs=['-cni-network','zbmarker'+str(node),'-cni-conf-dir',net/'conf','-cni-cache-dir',net/'cache','-cni-bin-dir',net/'bin']
        for src in sorted((stage/task).iterdir()):
            short=src.name[:8]
            if mode.startswith('deep'):
                if short!='08ae20d8':continue
                prior=Path('/tmp/zb-resume-deep-01')/(short+'-'+variant+'-deep')
            else:
                prior=extra/'results'/(short+'-'+variant)
            if passed(prior):continue
            target=stage/(short+'-'+mode)
            if target.exists():raise FileExistsError(target)
            args=['-trajectory',src/'trajectory.json','-rollout',src/'rollout.jsonl','-namespace','aries','-socket',sock,
                  '-checkpoint-dir',runtime/'checkpoints','-shim-dir',runtime/'shim','-io-dir','/run/'+unit+'/aries-io',
                  '-image',image,'-codex-tools',bundle/'codex-harbor-tools.tar.gz','-os-tools',stage/'dna-assembly-debs.tar.gz',*netargs]
            if node==191:args.append('-native-checkpoint')
            with target.with_suffix('.log').open('w') as log:
                subprocess.run([runtime/'bin/firecracker-ctr','-a',sock,'-n','aries','images','pull','--local','--platform','linux/amd64','--snapshotter','devmapper',image],check=True,stdout=log,stderr=log)
                rc=subprocess.run([sys.executable,stage/'vmm_exclusivity.py',str(target)+'-placement.jsonl','--',tracer/'run-traced.sh',stage/'aries',tracer,target,*args],env=env,stdout=log,stderr=log).returncode
            target.mkdir(exist_ok=True)
            (target/'driver.exit').write_text(str(rc)+'\n')
            shutil.copy2(runtime/'runtime.json',target/'runtime.json')
