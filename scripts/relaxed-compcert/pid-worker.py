"""Use the signed ordinary compiler fixture with the source-bound PID lifetime diagnostic."""
import hashlib
from pathlib import Path
import subprocess
STAGE=Path('/local/zb-relaxed-compcert-01')
TRIAL='921ac918-1cd2-41af-858a-a37490ced8f4'
BINARY='3ba2b8fe238845d2b1ea7e9ce8bbc4a924d69e5858a99dc3b9338dcdc49cce9d'
def render(data):
    assert hashlib.sha256(data).hexdigest()=='ee51d9265b39b1c79ea21e0e0ab5613d0017139eef79a706d8ec370aa71d0a86'
    s=data.decode()
    for old,new in [('out="$stage/$variant"','out="$stage/pid-witness-v1-01"'),
                    ('$stage/aries-relaxed-compcert','$stage/aries-921a-pid-lifetime'),
                    ('-diagnostic-process-drift -trajectory','-diagnostic-process-drift -diagnostic-opam-pid -trajectory')]:
        assert s.count(old)==1;s=s.replace(old,new)
    return s
if __name__=='__main__':
    assert Path('/var/emulab/boot/nodeid').read_text().strip()=='amd182'
    assert hashlib.sha256((STAGE/'aries-921a-pid-lifetime').read_bytes()).hexdigest()==BINARY
    args=['bash','-s','--',str(STAGE),'v1','fcmod-zepbound-amd182','/local/zb-validation/runtime','/local/zb-deep-kv-v1-amd182/cni',
          '/local/zb-torch-deep-v1-amd182/assets/codex-harbor-tools.tar.gz',TRIAL]
    subprocess.run(args,input=render((STAGE/'run-series.sh').read_bytes()),text=True,check=True)
