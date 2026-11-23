"""Run and archive the two scoped follow-ups after amd200's explicit terminal handoff."""
import argparse
import hashlib
import json
from pathlib import Path
import shlex
import subprocess
import tarfile
import time

REMOTE='/local/zb-e047-hostname-03'
BUNDLE='f29bbf06af45501694d950b8a6a2d357a6f1fc0b38d9d034a665e38b96fb4c86'
def ssh(*args,**kw):
    return subprocess.run(['ssh','-o','BatchMode=yes','-o','ConnectTimeout=15','nehalem@amd200.utah.cloudlab.us',shlex.join(list(map(str,args)))],check=True,**kw)
def sha(path):
    with path.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
def state(unit):
    return ssh('systemctl','show',unit,'-p','ActiveState','--value',capture_output=True,text=True).stdout.strip()
def main(root):
    cache=root/'repo/ARIES/.cache'
    release=cache/'harbor/e047-hostname-03.release.json'
    while not release.is_file():time.sleep(15)
    prior=json.loads(release.read_bytes());archive=cache/'zb-raw/resumed/amd200/marker-headers/ba8d88f7-v4-worker.tar.gz'
    assert prior['previous_invocation']=='4fdbca3eb86546cab169bd95690950d3' and prior['previous_sha256']==sha(archive)
    assert state('zb-marker-headers-v4-05')=='inactive'
    package=cache/'harbor/e047-hostname-03.tar.gz';assert sha(package)==BUNDLE
    ssh('sudo','-n','mkdir','-p',REMOTE)
    staged=ssh('sudo','-n','sh','-c','test -f "$1" && echo yes || echo no','sh',REMOTE+'/SHA256SUMS',capture_output=True,text=True).stdout.strip()
    if staged=='no':
        with package.open('rb') as f:ssh('sudo','-n','tee',REMOTE+'/package.tar.gz',stdin=f,stdout=subprocess.DEVNULL)
        assert ssh('sudo','-n','sha256sum',REMOTE+'/package.tar.gz',capture_output=True,text=True).stdout.split()[0]==BUNDLE
        ssh('sudo','-n','tar','-C',REMOTE,'-xzf',REMOTE+'/package.tar.gz')
    raw=cache/'zb-raw/e047-hostname';raw.mkdir(exist_ok=True,parents=True)
    assets=cache/'harbor/e047-hostname-03'
    for arm in ('v2','v4'):
        old=raw/f'e047ddea-{arm}-relaxed-ml-03'
        events=[json.loads(l) for l in (old/'raw/events.jsonl').open()]
        assert not any(e['event'] in ('session_start','tool_dispatch','snapshot_start') for e in events)
        assert events[-1]['event']=='removed' and "Read-only file system: '/etc/hosts'" in (old/'driver.log').read_text()
    for name in ('aries-e047-04','worker-04.py','SHA256SUMS-04'):
        local=assets/name
        with local.open('rb') as f:ssh('sudo','-n','tee',REMOTE+'/'+name,stdin=f,stdout=subprocess.DEVNULL)
        assert ssh('sudo','-n','sha256sum',REMOTE+'/'+name,capture_output=True,text=True).stdout.split()[0]==sha(local)
    ssh('sudo','-n','chmod','0755',REMOTE+'/aries-e047-04')
    for arm in ('v2','v4'):
        rid=f'e047ddea-{arm}-relaxed-ml-04';unit='zb-e047-hostname-'+arm+'-04'
        marker=raw/(rid+'.sha256')
        if marker.exists():continue
        started=ssh('sudo','-n','sh','-c','test -d "$1" && echo yes || echo no','sh',REMOTE+'/'+rid,capture_output=True,text=True).stdout.strip()
        if started=='no' and state(unit)=='inactive':
            ssh('sudo','-n','systemd-run','--unit='+unit,'--property=RuntimeMaxSec=2700','/usr/bin/python3','-B',REMOTE+'/worker-04.py',arm)
        while state(unit) in ('active','activating','deactivating'):time.sleep(20)
        status=state(unit)
        if status!='inactive':raise RuntimeError(f'{unit} {status}; inspect native command preflight before any recovery')
        ssh('sudo','-n','test','-f',REMOTE+'/'+rid+'/deployment-after.json')
        part=raw/(rid+'.partial.tar.gz');out=raw/(rid+'.tar.gz')
        with part.open('wb') as f:ssh('sudo','-n','tar','-C',REMOTE,'-I','gzip -1','-cf','-',rid,stdout=f)
        with tarfile.open(part) as t:
            assert t.getmember(rid+'/driver.exit').isfile();t.extractall(raw,filter='data')
        part.replace(out);marker.write_text(sha(out)+'  '+out.name+'\n')
        print(rid,marker.read_text().strip(),flush=True)
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root',type=Path);main(p.parse_args().root.resolve())
