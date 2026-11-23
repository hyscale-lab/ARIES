"""Replay the native launch-timing discrepancies with first and eventual outcomes retained."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

stage = Path(__file__).resolve().parent
node = int(sys.argv[1])
config = {
    203: ('zb-sanitize-01', ['v1'], '/local/zb-corpus-pypi-cohort-v1', '/local/zb-corpus-pypi-cohort-v1/cni'),
    191: ('zb-sanitize-01', ['v3','v4'], '/local/zb-fc-v4/pilot/pypi-cohort-amd191', '/local/zb-fc-v4/pilot/amd191-pypi-cni'),
    200: ('zb-cancel-async-amd200-01',['v2'], '/local/zb-corpus-financeapt-cohort-v2','/local/zb-corpus-financeapt-cohort-v2/cni'),
}
predecessor, variants, bundle, net = config[node]
bundle,net = Path(bundle),Path(net)
while subprocess.run(['systemctl','is-active','--quiet',predecessor]).returncode==0:
    time.sleep(10)
sys.path.insert(0,str(bundle))
sources=json.loads((stage/'sources.json').read_text())
for variant in variants:
    if node==203:
        unit,runtime='fcmod-zepbound-demand-amd203',Path('/users/nehalem/zepbound-demand-amd203/runtime')
    elif node==200:
        unit,runtime='fcmod-zepbound-amd200',Path('/local/zb-v2/runtime')
    else:
        unit,runtime='fcmod-zepbound-amd191-'+variant,Path('/local/zb-fc-v4')/variant/'runtime'
    sock='/run/'+unit+'/containerd.sock'
    trace=bundle/'tracer'
    env=dict(os.environ,ZB_VMM_CENSUS='1',ZB_CHECKPOINT_NAMESPACE_DIR=str(runtime/'checkpoints/aries'))
    if node==203:
        tools='/users/nehalem/zepbound-demand-amd203/tools/bpf/usr'
        env.update(PATH=tools+'/bin:'+env['PATH'],LD_LIBRARY_PATH=tools+'/lib/x86_64-linux-gnu')
    for row in sources:
        task,trial,image=row['task'],row['trial'],row['image']
        if task=='kv-store-grpc' and variant!='v1':
            continue
        src=stage/'sources'/trial
        target=stage/(trial[:8]+'-'+variant)
        if target.exists(): raise FileExistsError(target)
        args=['-timing-audit','-trajectory',src/'trajectory.json','-rollout',src/'rollout.jsonl','-namespace','aries',
              '-socket',sock,'-checkpoint-dir',runtime/'checkpoints','-shim-dir',runtime/'shim','-io-dir','/run/'+unit+'/aries-io',
              '-image',image,'-codex-tools',bundle/'codex-harbor-tools.tar.gz']
        if node==191:args.append('-native-checkpoint')
        if task=='overfull-hbox':
            args.extend(['-os-tools',stage/'overfull-hbox-debs.tar.gz'])
        else:
            network=json.loads(next((net/'conf').glob('*.conflist')).read_text())['name']
            args.extend(['-cni-network',network,'-cni-bin-dir',net/'bin','-cni-cache-dir',net/'cache','-cni-conf-dir',net/'conf'])
            if task=='kv-store-grpc':
                args.extend(['-os-tools',bundle/'kv-python.tar.gz'])
            else:
                args.extend(['-os-tools',bundle/'financial-document-processor-debs.tar.gz','-image-helper',stage/'aries-harbor-image'])
        with target.with_suffix('.log').open('w') as log:
            subprocess.run([runtime/'bin/firecracker-ctr','-a',sock,'-n','aries','images','pull','--local','--platform','linux/amd64','--snapshotter','devmapper',image],check=True,stdout=log,stderr=log)
            rc=subprocess.run([sys.executable, stage/'vmm_exclusivity.py',str(target)+'-placement.jsonl','--',trace/'run-traced.sh',stage/'aries',trace,target,*args],env=env,stdout=log,stderr=log).returncode
        target.mkdir(exist_ok=True)
        (target/'driver.exit').write_text(str(rc)+'\n')
        shutil.copy2(runtime/'runtime.json',target/'runtime.json')
