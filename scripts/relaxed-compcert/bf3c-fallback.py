"""Make the authenticated upstream of the source-pinned 20230608 OPAM archive reachable only during bf3c."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess

HOST='gitlab.inria.fr'
IP='128.93.193.18'
URL='https://gitlab.inria.fr/fpottier/menhir/-/archive/20230608/archive.tar.gz'
SHA512='334b9dcb1283a28b8547082a89536b1d439ff588290b8eaecdf4802c5f74dbc8d16ad6fc6c0820036183518d83e2cc273a75787a8b41137424c8e7ee82e2b50a'
RULE=['ZB183CCSNAP','-d',IP,'-p','tcp','--dport','443','-m','comment','--comment','zb-bf3c-menhir-upstream','-j','RETURN']
DNS_UNIT='zb-ccsnap-dns-amd183'
HOSTS=Path('/run')/DNS_UNIT/'hosts'
def rule(action):
    return subprocess.run(['iptables','-w','-t','mangle',action,*RULE],capture_output=True)
def main(action,receipt):
    backup=receipt/'dns-before'
    if action=='enable':
        receipt.mkdir(exist_ok=False)
        assert rule('-C').returncode==1
        old=HOSTS.read_text()
        assert HOST not in old
        shutil.copyfile(HOSTS,backup)
        response=subprocess.check_output(['curl','--noproxy','*','--fail','--silent','--show-error','--max-time','35','--resolve',f'{HOST}:443:{IP}',URL])
        assert len(response)==1340454 and hashlib.sha512(response).hexdigest()==SHA512
        rule('-I').check_returncode()
        HOSTS.write_text(old.rstrip()+'\n'+IP+' '+HOST+'\n')
        subprocess.run(['systemctl','kill','--signal=HUP',DNS_UNIT],check=True)
        rule('-C').check_returncode()
        (receipt/'verified.json').write_text(json.dumps(dict(source_url=URL,server_name=HOST,pinned_public_ipv4=IP,
            verified_sha512=SHA512,verified_bytes=len(response),private_dns=HOSTS.read_text(),scoped_rule=RULE),indent=2)+'\n')
    else:
        if backup.exists():
            shutil.copyfile(backup,HOSTS)
            subprocess.run(['systemctl','kill','--signal=HUP',DNS_UNIT],check=True)
        if rule('-C').returncode==0:rule('-D').check_returncode()
        assert rule('-C').returncode==1
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('action',choices=['enable','disable']);p.add_argument('receipt',type=Path)
    a=p.parse_args();main(a.action,a.receipt)
