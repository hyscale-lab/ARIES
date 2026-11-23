"""Use the verified wide-PFN amd201 v1 stack once the completed v4 original has been archived."""
import argparse
import datetime
import json
from pathlib import Path
import runpy
import subprocess

HERE=Path(__file__).resolve().parent; a=runpy.run_path(str(HERE/'relocate-deep.py'))
q=runpy.run_path(str(HERE/'deep-queue.py'));NODE='amd201';TRIAL=q['TRIAL'];STAGE=q['REMOTE']
def main(root):
    results=root/'plan/result/1_opportunity';raw=root/'repo/ARIES/.cache/zb-raw/relaxed-compcert-deep/v4'
    assert (raw/'worker.sha256').is_file() and a['local_state']('zb-relaxed-compcert-deep-v4-amd201')=='inactive'
    assert q['state'](NODE,'zb-relaxed-compcert-deep-v4-01')=='inactive'
    companion=results/'replacements/log-summary-date-ranges/relocated-modes.json';record=json.loads(companion.read_bytes())
    p,=[v for v in record['profiles'] if (v['level'],v['variant'])==('deep','v1')]
    historical=json.loads((results/'deep-sweep/compcert-v1.json').read_bytes())
    assert p['worker']==NODE and all(sig==historical['deployed_files'][k]['sha256'] for k,sig in p['source_sha256'].items())
    rt,unit=p['runtime'],p['service'];ctr=[rt+'/bin/firecracker-ctr','-a','/run/'+unit+'/containerd.sock','-n','aries']
    image=historical['source_image'];out=a['output'];sudo=a['sudo']
    if image not in out(*ctr,'images','ls','-q').splitlines(): sudo(*ctr,'images','pull','--local','--platform','linux/amd64','--snapshotter','devmapper',image)
    row,=[x for x in out(*ctr,'images','ls').splitlines() if x.split() and x.split()[0]==image];assert row.count(image.split('@')[1])>=2
    assert not out('ip','-o','route','show','10.239.183.0/28')
    assert q['state']('amd182','zb-relaxed-compcert-deep-v1-01')=='inactive'
    q['cmd']('amd182','sudo','-n','test','!','-e',STAGE+'/deep-v1-01')
    previous='zb-relaxed-compcert-deep-queue-v1'
    subprocess.run(['systemctl','--user','stop',previous],check=True)
    assert a['local_state'](previous)=='inactive'
    dest=results/'relaxed-compcert/deep/relocated-v1.json';assert not dest.exists()
    dest.write_text(json.dumps(dict(utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),active_node=NODE,previous_node='amd182',
        source_trial=TRIAL,active_controller='zb-relaxed-compcert-deep-v1-amd201',old_controller_state='inactive',superseded_controller=previous,
        original_worker_had_no_dispatch=True,source_image_readback=row,exact_runtime=p,stage_archive_sha256=a['BUNDLE_SHA'],
        cni_manifest_sha256=recorded('cni_manifest_sha256',results),codex_tools_sha256=recorded('codex_tools_sha256',results),
        verified_host_evidence=str(companion.relative_to(root)),host_evidence_sha256=q['sha'](companion),
        previous_deep_v4_archive_sha256=(raw/'worker.sha256').read_text().split()[0],source_requested={'vcpus':2,'memory_mib':4096,'storage_mib':10240}),indent=2)+'\n')
def recorded(key,results):
    return json.loads((results/'relaxed-compcert/deep/relocated-v4.json').read_bytes())[key]
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root',type=Path);main(p.parse_args().root.resolve())
