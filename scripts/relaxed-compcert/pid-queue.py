"""Run a distinct 921a PID diagnostic after existing amd182 deployments and verified deep export."""
import argparse
import hashlib
import json
from pathlib import Path
import shlex
import subprocess
import tarfile
import time

STAGE='/local/zb-relaxed-compcert-01'
ID='921ac918-1cd2-41af-858a-a37490ced8f4'
UNIT='zb-relaxed-compcert-pid-witness-v1-01'
HOST='nehalem@amd182.utah.cloudlab.us'
def ssh(*args,**kw):return subprocess.run(['ssh','-o','BatchMode=yes','-o','ConnectTimeout=15',HOST,shlex.join(args)],check=True,**kw)
def sha(p):
    with p.open('rb') as f:return hashlib.file_digest(f,'sha256').hexdigest()
def main(root):
    cache=root/'repo/ARIES/.cache'
    while not (cache/'zb-raw/relaxed-compcert-deep/v1/worker.sha256').is_file():time.sleep(40)
    # A deep prerequisite can be relocated while native ordinary replay still owns amd182.
    while not (cache/'harbor/log-summary-replacement/amd182.complete.json').is_file() or not (cache/'zb-raw/relaxed-compcert/v4/extracted').is_file():time.sleep(40)
    assert ssh('systemctl','show','-p','ActiveState','--value','zb-relaxed-compcert-v4',capture_output=True).stdout.strip()==b'inactive'
    original=json.loads((root/'plan/result/1_opportunity/relaxed-compcert/runs/921ac918-v1/receipt.json').read_bytes())
    assert original['source_trial']==ID and original['source_complete'] and not original['compiler_artifact_verified']
    paths={cache/'harbor/relaxed-compcert-pid/aries-pid-lifetime':'aries-921a-pid-lifetime',Path(__file__).with_name('pid-worker.py'):'pid-worker-01.py'}
    for src,dst in paths.items():
        with src.open('rb') as f:ssh('sudo','-n','tee',STAGE+'/'+dst,stdin=f,stdout=subprocess.DEVNULL)
        assert ssh('sudo','-n','sha256sum',STAGE+'/'+dst,capture_output=True,text=True).stdout.split()[0]==sha(src)
    ssh('sudo','-n','chmod','0755',STAGE+'/aries-921a-pid-lifetime')
    folder='pid-witness-v1-01'
    done=STAGE+'/'+folder+'/series-status.txt'
    def check():
        return ssh('sudo','-n','sh','-c','test -s "$1" && echo yes || echo no','sh',done,capture_output=True).stdout.strip()==b'yes'
    state=lambda:ssh('systemctl','show','-p','ActiveState','--value',UNIT,capture_output=True).stdout.strip()
    if not check() and state()==b'inactive':
        ssh('sudo','-n','systemd-run','--unit='+UNIT,'/usr/bin/python3','-B',STAGE+'/pid-worker-01.py')
    while True:
        current=state()
        if current==b'failed':raise RuntimeError(UNIT+' failed before terminal receipts')
        if current==b'inactive' and check():break
        time.sleep(30)
    out=cache/'zb-raw/relaxed-compcert-pid/v1';out.mkdir(parents=True,exist_ok=True)
    part=out/'worker.partial.tar.gz';arc=out/'worker.tar.gz'
    with part.open('wb') as f:ssh('sudo','-n','tar','-C',STAGE,'-I','gzip -1','--exclude='+folder+'/cni/bin','--exclude='+folder+'/cni/cache','--exclude='+folder+'/cni/ipam','-cf','-',folder,'inputs.sha256','pid-worker-01.py',stdout=f)
    with tarfile.open(part) as t:
        status=t.extractfile(folder+'/series-status.txt').read().decode().split()
        assert status[:3]==['amd182','v1',ID];int(status[3]);t.extractall(out,filter='data')
    part.rename(arc);(out/'worker.sha256').write_text(sha(arc)+'  worker.tar.gz\n')
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root',type=Path);main(p.parse_args().root.resolve())
