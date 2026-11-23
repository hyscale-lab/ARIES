"""Require both old placement acknowledgments before starting the paired log campaign."""
import argparse
import json
from pathlib import Path
import runpy
import subprocess
import sys
import time

HERE=Path(__file__).resolve().parent
q=runpy.run_path(str(HERE/'queue.py'))
def main(root,node):
    aries=root/'repo/ARIES'; cache=aries/'.cache'
    if node=='amd183':
        decision=root/'plan/result/1_opportunity/replacements/log-summary-date-ranges/relocated-v2-v3.json'
        q['wait']('completed verified amd183 native and reporting profiles',decision.is_file)
        ack=cache/'harbor/log-summary-replacement/amd189.v23-skip.json'
        if not ack.is_file():subprocess.run(['systemctl','--user','restart','zb-log-summary-queue-amd189'],check=True)
        q['wait']('verified original amd189 v2/v3 skips',ack.is_file)
        data=json.loads(ack.read_bytes())
        assert data['decision_sha256']==q['digest'](decision) and set(map(tuple,data['skipped']))=={('ordinary','v2'),('ordinary','v3')}
        local='zb-log-summary-queue-amd183'
        state=subprocess.check_output(['systemctl','--user','show',local,'-p','ActiveState','--value'],text=True).strip()
        if state=='inactive' and not (cache/'harbor/log-summary-replacement/amd183.complete.json').is_file():
            subprocess.run(['systemd-run','--user','--unit='+local,'--property=WorkingDirectory='+str(root),sys.executable,'-Bu',HERE/'queue.py',root,node],check=True)
        else:assert state=='active' or (cache/'harbor/log-summary-replacement/amd183.complete.json').is_file()
        return
    previous=cache/'zb-raw/relaxed-compcert-deep/v4/worker.sha256'
    q['wait']('authenticated completed previous deep v4 export',lambda:previous.is_file() and q['status']('amd201','zb-relaxed-compcert-deep-v4-01')=='inactive')
    result=root/'plan/result/1_opportunity';decision=result/'replacements/log-summary-date-ranges/relocated-modes.json'
    if not decision.exists():subprocess.run([sys.executable,'-B',HERE/'relocate.py',root],check=True)
    for oldnode in ('amd182','amd189'):
        ack=cache/'harbor/log-summary-replacement'/(oldnode+'.skip.json')
        if not ack.exists():
            subprocess.run(['systemctl','--user','restart','zb-log-summary-queue-'+oldnode],check=True)
        q['wait']('durable superseded-mode acknowledgement from '+oldnode,lambda:ack.is_file())
        data=json.loads(ack.read_bytes())
        expected={(l,v) for l,v,_,_ in q['SERIES'][oldnode] if v in ('v1','v4')}
        assert data['node']==oldnode and set(map(tuple,data['skipped']))==expected and data['decision_sha256']==q['digest'](decision)
    cc=aries/'scripts/relaxed-compcert';relocated=result/'relaxed-compcert/deep/relocated-v1.json'
    if not relocated.exists():subprocess.run([sys.executable,'-B',cc/'relocate-baseline.py',root],check=True)
    assert json.loads(relocated.read_bytes())['active_node']=='amd201'
    runners=[('zb-relaxed-compcert-deep-v1-amd201',cc/'deep-queue.py',[root,'v1','--relocated-amd201'],cache/'zb-raw/relaxed-compcert-deep/v1/worker.sha256'),
             ('zb-log-summary-queue-amd201',HERE/'queue.py',[root,'amd201'],cache/'harbor/log-summary-replacement/amd201.complete.json')]
    for unit,script,args,receipt in runners:
        state=subprocess.check_output(['systemctl','--user','show',unit,'-p','ActiveState','--value'],text=True).strip()
        if state=='inactive' and not receipt.exists():
            subprocess.run(['systemd-run','--user','--unit='+unit,'--property=WorkingDirectory='+str(root),sys.executable,'-Bu',script,*args],check=True)
        else: assert state=='active' or receipt.exists()
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root',type=Path);p.add_argument('--node',choices=('amd201','amd183'),default='amd201');args=p.parse_args();main(args.root.resolve(),args.node)
