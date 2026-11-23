"""Gate, submit and export exactly one source-bound deep CompCert completion attempt."""
import argparse
from datetime import datetime,timezone
import hashlib
import json
from pathlib import Path
import shlex
import subprocess
import tarfile
import time

HERE=Path(__file__).resolve().parent
REMOTE='/local/zb-relaxed-compcert-01'
TRIAL='1d11a94f-d715-44d6-9ff6-1f097a8793f8'
def sha(p):
    with p.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
def read(p):return json.loads(p.read_bytes())
def cmd(node,*args,capture=False,**kwargs):
    return subprocess.run(['ssh','-o','ConnectTimeout=15','-o','BatchMode=yes',f'nehalem@{node}.utah.cloudlab.us',shlex.join(args)],capture_output=capture,check=True,**kwargs)
def state(node,unit):
    return cmd(node,'systemctl','show','-p','ActiveState','--value',unit,capture=True).stdout.decode().strip()
def exists(node,path):
    return cmd(node,'sudo','-n','sh','-c','test -s "$1" && echo yes || echo no','sh',path,capture=True).stdout.strip()==b'yes'
def wait(name,check):
    print(name,flush=True)
    while True:
        try:
            if check():return
        except subprocess.CalledProcessError as e:
            if e.returncode!=255:raise
        time.sleep(30)
def main(root,variant,relocated=False):
    node={'v1':'amd182','v4':'amd189'}[variant]
    result=root/'plan/result/1_opportunity'
    old=read(result/'deep-sweep'/f'compcert-{variant}.json')
    assert old['source_trial']==TRIAL
    if old['driver_exit']==0 and old['source_session_end']:
        print('Prior complete',variant,flush=True)
        return
    assert old['first_divergent_step']==6 and old['driver_exit']==1
    cache=root/'repo/ARIES/.cache'
    placement=result/f'relaxed-compcert/deep/relocated-{variant}.json'
    if relocated:
        assert variant in ('v1','v4') and read(placement)['active_node']=='amd201' and read(placement)['source_trial']==TRIAL
        node='amd201'
        oldunit='zb-relaxed-compcert-deep-queue-'+variant
        assert subprocess.check_output(['systemctl','--user','show',oldunit,'-p','ActiveState','--value'],text=True).strip()=='inactive'
        assert state({'v1':'amd182','v4':'amd189'}[variant],f'zb-relaxed-compcert-deep-{variant}-01')=='inactive'
    elif placement.is_file():
        assert read(placement)['active_node']=='amd201' and read(placement)['source_trial']==TRIAL
        print('Superseded by unique amd201 '+variant+' submission',flush=True)
        return
    else:
        prior=cache/'harbor/log-summary-replacement'/f'{node}.complete.json'
        wait('Wait for complete ordinary and deep log series and transfer on '+node,lambda:prior.is_file() and len(read(prior))==3)
        if variant=='v4':
            hf='/tmp/zb-timing17-01/b615a3ec-v3/driver.exit'
            wait('Wait for native b615 HF terminal receipt and released amd189',lambda:exists(node,hf) and state(node,'zb-timing17-hf') in ('inactive','failed'))
    output=f'deep-{variant}-01'
    series=f'{REMOTE}/{output}/series-status.txt'
    unit=f'zb-relaxed-compcert-deep-{variant}-01'
    if not exists(node,series) and state(node,unit)=='inactive':
        script=HERE/'deep-worker.py'
        worker=f'{REMOTE}/deep-worker-01.py'
        data=script.read_bytes()
        cmd(node,'sudo','-n','tee',worker,input=data,stdout=subprocess.DEVNULL)
        actual=cmd(node,'sudo','-n','sha256sum',worker,capture=True).stdout.decode().split()[0]
        assert actual==sha(script)
        cmd(node,'sudo','-n','systemd-run','--unit='+unit,'/usr/bin/python3','-B',worker,variant)
    def done():
        current=state(node,unit)
        if current=='failed':raise RuntimeError(f'{node} {unit} failed, inspect journal')
        return current=='inactive' and exists(node,series)
    wait('Wait for terminal source and clean worker receipt',done)
    local=cache/'zb-raw/relaxed-compcert-deep'/variant
    local.mkdir(parents=True,exist_ok=True)
    partial=local/'worker.partial.tar.gz';archive=local/'worker.tar.gz'
    if not archive.is_file():
        with partial.open('wb') as stream:
            cmd(node,'sudo','-n','tar','-C',REMOTE,'-I','gzip -1','--exclude='+output+'/cni/bin','--exclude='+output+'/cni/cache','--exclude='+output+'/cni/ipam','-cf','-',output,'inputs.sha256','deep-worker-01.py',stdout=stream)
        with tarfile.open(partial) as data:
            s=data.extractfile(output+'/series-status.txt').read().decode().split()
            assert s[:3]==[node,variant,TRIAL] and len(s)==4
            int(s[3])
        partial.rename(archive)
    with tarfile.open(archive) as data:data.extractall(local,filter='data')
    (local/'worker.sha256').write_text(sha(archive)+'  worker.tar.gz\n')
    print('Raw archive verified',archive,flush=True)
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root',type=Path);p.add_argument('variant',choices=('v1','v4'))
    p.add_argument('--relocated-amd201',action='store_true');a=p.parse_args();main(a.root.resolve(),a.variant,a.relocated_amd201)
