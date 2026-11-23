"""Run the reviewed hostname-calibrated e047 original on its original stock amd200 arm."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess

STAGE=Path('/local/zb-e047-hostname-03')
IMAGE='docker.io/alexgshaw/torch-tensor-parallelism@sha256:c50ac169e11465f41b49749fe87e4487f24d31a0c39c0906cd3242cc5499c690'
CNI=Path('/local/zb-corpus-torch-cohort-4b58-v2/cni')
def invoke(args,**kw):return subprocess.run([str(x) for x in args],check=True,**kw)
def sha(path):
    with path.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
def idle():
    for f in Path('/proc').glob('[0-9]*/comm'):
        try:name=f.read_text().strip()
        except (FileNotFoundError,ProcessLookupError):continue
        if name in ('firecracker','.firecracker-re'):raise RuntimeError(f'other VMM present at {f}')
def main(arm):
    assert Path('/var/emulab/boot/nodeid').read_text().strip()=='amd200'
    assert Path('/sys/devices/system/cpu/smt/active').read_text().strip()=='0'
    rt,unit={'v2':('/local/zb-v2/runtime','fcmod-zepbound-amd200'),
             'v4':('/local/zb-relaxed-ml-01/stack-v4/runtime','fcmod-zepbound-relaxed-ml-v4')}[arm]
    rt=Path(rt);socket='/run/'+unit+'/containerd.sock'
    invoke(['sha256sum','--quiet','-c',STAGE/'SHA256SUMS'],cwd=STAGE)
    invoke(['sha256sum','--quiet','-c',STAGE/'SHA256SUMS-04'],cwd=STAGE)
    idle()
    out=STAGE/('e047ddea-'+arm+'-relaxed-ml-04');out.mkdir()
    previous=json.loads((STAGE/(arm+'-deployment.json')).read_bytes())
    def deployment(phase):
        actual=json.loads(subprocess.check_output(['python3','-B',STAGE/'audit-idle-runtime.py',unit,rt]))
        (out/f'deployment-{phase}.json').write_text(json.dumps(actual,indent=2)+'\n')
        assert actual['node']=='amd200'
        assert {k:d['sha256'] for k,d in actual['files'].items()}=={k:d['sha256'] for k,d in previous['files'].items()},'deployment contents differ'
        assert all(not actual[k]['vmm_processes'] and not any(actual[k]['tasks_by_socket'].values()) for k in ('before','after'))
    deployment('before')
    config=rt/'runtime.json';(out/'runtime-config.json').write_bytes(config.read_bytes())
    cni=CNI/'conf/10-torch.conflist'
    assert sha(cni)=='e82ae4a08d96e36343838c07a03b3aadfea8abf01db337be2c6397bcc233a1aa'
    assets=[STAGE/'torch-tensor-parallelism-debs.tar.gz',STAGE/'codex-harbor-tools.tar.gz',cni]
    (out/'assets.sha256').write_text(''.join(sha(f)+'  '+str(f)+'\n' for f in assets))
    cli=STAGE/'aries-e047-04'
    (out/'executed-adapter.sha256').write_text(sha(cli)+'  '+str(cli)+'\n')
    cpu=subprocess.check_output(['systemctl','show',unit,'-p','EffectiveCPUs','-p','AllowedCPUs','-p','CPUQuotaPerSecUSec'],text=True)
    assert 'EffectiveCPUs=0-15\n' in cpu and 'CPUQuotaPerSecUSec=infinity\n' in cpu
    (out/'host-capacity.txt').write_text(cpu)
    args=['-relaxed-ml','-e047-source-hostname','-guest-task-v3']
    if arm=='v4':args+=['-native-checkpoint']
    src=STAGE/'sources/e047ddea'
    args+=['-trajectory',src/'trajectory.json','-rollout',src/'rollout.jsonl','-image',IMAGE,'-namespace','aries','-socket',socket,
           '-checkpoint-dir',rt/'checkpoints','-shim-dir',rt/'shim','-io-dir','/run/'+unit+'/aries-io','-codex-tools',assets[1],'-os-tools',assets[0],
           '-vcpus','1','-memory-mib','8192','-storage-mib','10240','-cni-network','zbtorch200','-cni-conf-dir',CNI/'conf','-cni-cache-dir',CNI/'cache','-cni-bin-dir',CNI/'bin']
    (out/'argv.shell').write_text(shlex.join(map(str,args))+'\n')
    env=dict(os.environ,ZB_VMM_CENSUS='1',BPFTRACE_PERF_RB_PAGES='1024')
    if arm=='v2':env['ZB_CHECKPOINT_NAMESPACE_DIR']=str(rt/'checkpoints/aries')
    else:env.pop('ZB_CHECKPOINT_NAMESPACE_DIR',None)
    ctr=[rt/'bin/firecracker-ctr','-a',socket,'-n','aries','images','ls','-q']
    assert IMAGE in subprocess.check_output(ctr,text=True).splitlines()
    idle()
    trace=STAGE/'tracer'
    argv=['python3','-B',STAGE/'vmm_exclusivity.py',out/'placement.jsonl','--','bash',trace/'run-traced.sh',cli,trace,out/'raw',*args]
    with (out/'driver.log').open('w') as log:
        result=subprocess.run(list(map(str,argv)),cwd=STAGE,env=env,stdout=log,stderr=subprocess.STDOUT)
    (out/'driver.exit').write_text(str(result.returncode)+'\n')
    idle();deployment('after')
if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('variant',choices=['v2','v4']);main(parser.parse_args().variant)
