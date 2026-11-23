"""Launch one literal 1d11 compiler attempt with the full guest PFN observer on the assigned stock deployment."""
import argparse
import hashlib
import os
from pathlib import Path
import runpy
import tempfile

STAGE = Path('/local/zb-relaxed-compcert-01')
TRIAL = '1d11a94f-d715-44d6-9ff6-1f097a8793f8'
BASE_SHA = 'ee51d9265b39b1c79ea21e0e0ab5613d0017139eef79a706d8ec370aa71d0a86'
CLEAN = '292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f'
DEEP = '8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5'
DEPLOYMENTS = {
    'v1': ('amd182','fcmod-zepbound-tdeep182v1','/local/zb-torch-deep-v1-amd182/runtime','/local/zb-deep-kv-v1-amd182/cni'),
    'v4': ('amd189','fcmod-zepbound-amd189-video-deep','/local/zb-video-deep-v4-amd189/runtime','/local/zb-compcert-network/opam-cni'),
}
def render(src):
    assert hashlib.sha256(src).hexdigest()==BASE_SHA
    text=src.decode()
    for old,new in ((CLEAN,DEEP),('out="$stage/$variant"', 'out="$stage/deep-$variant-01"'),
                    ('export ZB_VMM_CENSUS=1 ', 'export ZB_CONTENT_SAMPLE=8192 ZB_GUEST_VSOCK=1 ZB_VMM_CENSUS=1 ')):
        assert text.count(old)==1
        text=text.replace(old,new)
    return text
def main(v):
    node,unit,runtime,plugins=DEPLOYMENTS[v]
    assert Path('/var/emulab/boot/nodeid').read_text().strip()==node
    assert not (STAGE/f'deep-{v}-01').exists()
    script=render((STAGE/'run-series.sh').read_bytes())
    # The original sealed driver, binary, process policy, CNI and source bytes remain pinned.
    args=['bash','-s','--',str(STAGE),v,unit,runtime,plugins,'/local/zb-replacement-log-summary-01/codex-harbor-tools.tar.gz',TRIAL]
    import subprocess
    subprocess.run(args,input=script,text=True,check=True)
    bridge_helper=STAGE/'bf3c-series-01.py'
    assert hashlib.sha256(bridge_helper.read_bytes()).hexdigest()=='c50b3f64dc05d6e8cca70214a0b3c915fd499abf61f287fd21fb601a4f1c6b60'
    runpy.run_path(str(bridge_helper))['retire_empty_bridge']()
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('variant',choices=DEPLOYMENTS);main(p.parse_args().variant)
