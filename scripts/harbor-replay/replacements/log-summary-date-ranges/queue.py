"""Serialize log-summary arms behind the assigned worker's CompCert measurements and raw transfer."""
import argparse
import csv
import hashlib
import json
from pathlib import Path
import shlex
import subprocess
import tarfile
import time

PAYLOAD = 'ae4fda771fbdb774e270f8c3501fb0e0cda9ec498f7b45f4c7e8b23b2125d088'
STAGE = '/local/zb-replacement-log-summary-01'
SERIES = {
    'amd201': [
        ('ordinary','v1','fcmod-zepbound-relaxed-ml-v1','/local/zb-relaxed-ml-01/stack-v1/runtime'),
        ('ordinary','v4','fcmod-zepbound-tex-v4','/local/zb-texpilot/v4/runtime'),
        ('deep','v1','fcmod-zepbound-cancel-async-deep-v1','/local/zb-cancel-async-01/deep-v1/runtime'),
        ('deep','v4','fcmod-zepbound-cancel-async-deep-v4','/local/zb-cancel-async-deep-v4-amd201/runtime')],
    'amd182': [
        ('ordinary', 'v1', 'fcmod-zepbound-amd182', '/local/zb-validation/runtime'),
        ('ordinary', 'v4', 'fcmod-zepbound-relaxed-cc-v4-182', '/local/zb-relaxed-compcert-v4-runtime/runtime'),
        ('deep', 'v1', 'fcmod-zepbound-tdeep182v1', '/local/zb-torch-deep-v1-amd182/runtime')],
    'amd189': [
        ('ordinary', 'v2', 'fcmod-zepbound-amd189-stockv2', '/local/zb-stock189-v2/runtime'),
        ('ordinary', 'v3', 'fcmod-zepbound-amd189-stockv3', '/local/zb-stock189-v3/runtime'),
        ('deep', 'v4', 'fcmod-zepbound-amd189-video-deep', '/local/zb-video-deep-v4-amd189/runtime')],
}
PREFIXES = ['aa46', '426e', 'dee8', '015e', '56d7']
def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()
def ssh(node, cmd, **kwargs):
    return subprocess.run(['ssh','-o','BatchMode=yes','-o','ConnectTimeout=15',f'nehalem@{node}.utah.cloudlab.us',cmd],check=True,**kwargs)
def privileged(*args):
    return shlex.join(['sudo','-n',*args])
def status(node, unit):
    r=ssh(node,shlex.join(['systemctl','show','--property=ActiveState','--value',unit]),capture_output=True,text=True)
    return r.stdout.strip()
def wait(prior, predicate):
    print('Waiting for',prior,flush=True)
    while True:
        try:
            if predicate():return
        except subprocess.CalledProcessError as error:
            if error.returncode != 255:raise
        time.sleep(40)
def archive(node, base, level, variant, expected):
    directory=f'{level}-{variant}'
    target=base/directory
    target.mkdir(exist_ok=True, parents=True)
    raw=target/'worker.tar.gz'
    if not raw.exists():
        part=raw.with_suffix('.partial.gz')
        with part.open('wb') as output:
            ssh(node, privileged('tar','-C',STAGE,'-I','gzip -1','-cf','-',directory,'inputs.sha256'),stdout=output)
        with tarfile.open(part) as stream:
            result=stream.extractfile(f'{directory}/series-status.txt').read().decode().splitlines()
            assert len(result)==len(expected)
            for line,prefix in zip(result,expected,strict=True):
                host,typ,var,trial,code=line.split()
                assert (host,typ,var)==(node,level,variant) and trial.startswith(prefix)
                int(code)
        part.rename(raw)
    with tarfile.open(raw) as stream:stream.extractall(target,filter='data')
    sha=digest(raw)
    (target/'worker.sha256').write_text(f'{sha}  worker.tar.gz\n')
    print('Archived',node,directory,sha,flush=True)
    return {'node':node,'level':level,'variant':variant,'worker_archive_sha256':sha,
            'statuses':(target/directory/'series-status.txt').read_text().splitlines()}
def main(root,node):
    cache=root/'repo/ARIES/.cache'
    local=cache/'harbor/log-summary-replacement'
    archive_path=local/'stage.tar.gz'
    assert digest(archive_path)==PAYLOAD
    decision=root/'plan/result/1_opportunity/replacements/log-summary-date-ranges/relocated-modes.json'
    moved=set()
    if decision.is_file():
        move=json.loads(decision.read_bytes());assert move['active_node']=='amd201'
        moved={(m['level'],m['variant']) for m in move['profiles']}
    if node=='amd201':
        assert moved=={(level,v) for level,v,_,_ in SERIES[node]}
        for prior_node in ('amd182','amd189'):
            ack=json.loads((local/(prior_node+'.skip.json')).read_bytes())
            assert ack['decision_sha256']==digest(decision) and ack['node']==prior_node
        wait('Complete previous compiler deep-baseline trace, worker exit and raw export',lambda:any((cache/'zb-raw/relaxed-compcert-deep/v1'/marker).is_file() for marker in ('transfer-verified.sha256','worker.sha256')) and status(node,'zb-relaxed-compcert-deep-v1-01')=='inactive')
    else:
        skipped=[(level,v) for level,v,_,_ in SERIES[node] if (level,v) in moved]
        if skipped:
            for level,v in skipped:
                assert status(node,f'zb-log-summary-{node}-{level}-{v}')=='inactive'
                ssh(node,privileged('test','!','-e',STAGE+f'/{level}-{v}'))
            (local/(node+'.skip.json')).write_text(json.dumps(dict(node=node,skipped=skipped,decision_sha256=digest(decision)),indent=2)+'\n')
        variant='v4' if node=='amd182' else 'v3'
        wait(f'CompCert {node}/{variant} verification and completed archive extraction',
             lambda:(cache/'zb-raw/relaxed-compcert'/variant/'extracted').is_file() and status(node,f'zb-relaxed-compcert-{variant}')=='inactive')
    remote=ssh(node,privileged('bash','-c',f'test -s {STAGE}/inputs.sha256 && echo ready || echo new'),capture_output=True,text=True).stdout.strip()
    require_local=any(node=='amd201' or (l,v) not in moved for l,v,_,_ in SERIES[node])
    if not require_local:pass
    elif remote=='new':
        ssh(node,privileged('mkdir','-p',STAGE))
        with archive_path.open('rb') as stream:
            ssh(node,'sudo -n tee '+STAGE+'/payload.tar.gz >/dev/null',stdin=stream)
        shell=f'cd {STAGE} && echo "{PAYLOAD}  payload.tar.gz" | sha256sum -c - && tar -xzf payload.tar.gz && sha256sum --quiet -c inputs.sha256'
        ssh(node,privileged('bash','-c',shell))
    elif remote!='ready':raise ValueError(remote)
    receipts=[]
    root_raw=cache/'zb-raw/log-summary-replacement'
    for level,variant,runtime_unit,runtime in SERIES[node]:
        if node!='amd201' and (level,variant) in moved:continue
        task=f'zb-log-summary-{node}-{level}-{variant}'
        ids=['aa46'] if level=='deep' else PREFIXES
        done=f'{STAGE}/{level}-{variant}/series-status.txt'
        shell=f'test -f {done} && wc -l < {done} || echo 0'
        def finished():
            n=int(ssh(node,privileged('bash','-c',shell),capture_output=True,text=True).stdout)
            active=status(node,task)
            if active=='failed':raise RuntimeError(f'{node}/{task} failed with {n} of {len(ids)} receipts')
            if active=='inactive' and n<len(ids):raise RuntimeError(f'{node}/{task} inactive with {n} receipts')
            return n==len(ids) and active=='inactive'
        begun=ssh(node,privileged('bash','-c',f'test -d {STAGE}/{level}-{variant} && echo yes || echo no'),capture_output=True,text=True).stdout.strip()
        if begun=='no' and status(node,task)=='inactive':
            ssh(node,privileged('systemd-run','--unit='+task,STAGE+('/run-series-amd201.sh' if node=='amd201' else '/run-series.sh'),STAGE,variant,level,runtime_unit,runtime,*ids))
        wait(f'{node} {level} {variant} {len(ids)} trials',finished)
        receipts.append(archive(node,root_raw,level,variant,ids))
    if node!='amd201' and any((l,v) in moved for l,v,_,_ in SERIES[node]):
        other=local/'amd201.complete.json'
        wait('Relocated unique v1/v4 log archives',other.is_file)
        for item in json.loads(other.read_bytes()):
            if any((item['level'],item['variant'])==(l,v) for l,v,_,_ in SERIES[node]):receipts.append(item)
        assert len(receipts)==len(SERIES[node])
    (local/f'{node}.complete.json').write_text(json.dumps(receipts,indent=2)+'\n')
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('root',type=Path);p.add_argument('node',choices=SERIES)
    a=p.parse_args();main(a.root.resolve(),a.node)
