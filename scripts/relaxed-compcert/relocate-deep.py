"""Verify a matching amd201 deep native stack before superseding the unstarted amd189 queue."""
import argparse
import datetime
import json
from pathlib import Path
import runpy
import subprocess

HERE=Path(__file__).resolve().parent
q=runpy.run_path(str(HERE/'deep-queue.py'))
cmd,state,sha=q['cmd'],q['state'],q['sha']
TRIAL=q['TRIAL']
NODE='amd201'
UNIT='fcmod-zepbound-cancel-async-deep-v4'
RT='/local/zb-cancel-async-deep-v4-amd201/runtime'
STAGE=q['REMOTE']
PREDECESSOR='zb-cancel-async-diagnostic-v4-amd201'
EXPORT='zb-cancel-async-collect-diagnostic-relocated-v4'
PRIOR='zb-relaxed-compcert-deep-queue-v4'
BUNDLE_SHA='a789359a15080b8d197304510c2ed3685df30bff70dd3d3cb13621cbedf26975'
def remote(*args,**kw):
    return cmd(NODE,*args,**kw)
def sudo(*args,**kw):
    return remote('sudo','-n',*args,**kw)
def output(*args):
    return sudo(*args,capture=True).stdout.decode().strip()
def local_state(unit):
    return subprocess.check_output(['systemctl','--user','show','-p','ActiveState','--value',unit],text=True).strip()
def stage(root):
    dest=root/'plan/result/1_opportunity/relaxed-compcert/deep/relocated-v4.json'
    assert not dest.exists()
    terminal=output('cat','/local/zb-cancel-async-02/retry-decisions-v4-relocated.tsv')
    assert len(terminal.splitlines())==2 and state(NODE,PREDECESSOR)=='inactive'
    collector=subprocess.check_output(['systemctl','show','-p','ActiveState','--value',EXPORT],text=True).strip()
    assert collector=='inactive' and local_state(EXPORT)=='inactive'
    assert state('amd189','zb-relaxed-compcert-deep-v4-01')=='inactive'
    cmd('amd189','sudo','-n','test','!','-e',STAGE+'/deep-v4-01')
    bundle=root/'repo/ARIES/.cache/harbor/relaxed-compcert/stage.tar.gz'
    assert sha(bundle)==BUNDLE_SHA
    sudo('mkdir','-p',STAGE)
    sudo('test','!','-e',STAGE+'/deep-v4-01')
    with bundle.open('rb') as f:sudo('tar','-C',STAGE,'-xzf','-',stdin=f)
    with (HERE/'bf3c-series.py').open('rb') as f:sudo('tee',STAGE+'/bf3c-series-01.py',stdin=f,stdout=subprocess.DEVNULL)
    sudo('bash','-c','set -eu; cd "$1"; sha256sum --quiet -c inputs.sha256; cd /local/zb-corpus-pypi-cohort-v3/cni/bin; sha256sum --quiet -c ../SHA256SUMS','sh',STAGE)
    verified=json.loads(output('python3','-B',STAGE+'/audit-idle-runtime.py',UNIT,RT))
    for phase in ('before','after'):
        assert not verified[phase]['vmm_processes'] and not any(verified[phase]['tasks_by_socket'].values())
    original=json.loads((root/'plan/result/1_opportunity/deep-sweep/compcert-v4.json').read_bytes())
    exact=['rootfs','kernel','firecracker','daemon','live_daemon','shim','ctr','runc','jailer','jailer_config']
    assert all(verified['files'][k]['sha256']==original['deployed_files'][k]['sha256'] for k in exact)
    profile=output('cat',RT+'/runtime.json')
    old=original['runtime_profile']
    oldroot=str(Path(old['root_drive']).parent)
    assert json.loads(profile.replace(RT,oldroot))==old
    address='/run/'+UNIT+'/containerd.sock'
    ctr=[RT+'/bin/firecracker-ctr','-a',address,'-n','aries']
    image=original['source_image']
    if image not in output(*ctr,'images','ls','-q').splitlines():
        sudo(*ctr,'images','pull','--platform','linux/amd64',image)
    present=output(*ctr,'images','ls')
    row,=[x for x in present.splitlines() if x.split() and x.split()[0]==image]
    assert row.count(image.split('@')[1])>=2
    hardware=json.loads(output('python3','-c',"""from pathlib import Path
import os,struct,json
from glob import glob
rows=[]
for i in range(16):
 c=Path('/sys/devices/system/cpu/cpu'+str(i))
 with open('/dev/cpu/'+str(i)+'/msr','rb',buffering=0) as f:
  hw=struct.unpack('<Q',os.pread(f.fileno(),8,0xc0010015))[0]
 rows.append({'cpu':i,'siblings':(c/'topology/thread_siblings_list').read_text().strip(),'hwcr':hex(hw),'boost_disabled':bool(hw&(1<<25))})
print(json.dumps({'cpus':rows,'smt':Path('/sys/devices/system/cpu/smt/active').read_text().strip(),'online':Path('/sys/devices/system/cpu/online').read_text().strip(),'boot_id':Path('/proc/sys/kernel/random/boot_id').read_text().strip()}))"""))
    assert hardware['smt']=='0' and hardware['online']=='0-15' and all(x['siblings']==str(x['cpu']) and x['boost_disabled'] for x in hardware['cpus'])
    cni_sha=output('sha256sum','/local/zb-corpus-pypi-cohort-v3/cni/SHA256SUMS').split()[0]
    assert cni_sha==sha(root/'repo/ARIES/.cache/zb-raw/relaxed-compcert/v1/v1/cni/SHA256SUMS')
    tools_sha=output('sha256sum','/local/zb-corpus-pypi-cohort-v3/codex-harbor-tools.tar.gz').split()[0]
    assert tools_sha=='1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab'
    subprocess.run(['systemctl','--user','stop',PRIOR],check=True)
    assert local_state(PRIOR)=='inactive' and state('amd189','zb-relaxed-compcert-deep-v4-01')=='inactive'
    dest.write_text(json.dumps(dict(utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),source_trial=TRIAL,active_node=NODE,previous_node='amd189',superseded_controller=PRIOR,old_controller_state='inactive',original_worker_had_no_dispatch=True,active_controller='zb-relaxed-compcert-deep-v4-amd201',stage_archive_sha256=BUNDLE_SHA,cni_manifest_sha256=cni_sha,codex_tools_sha256=tools_sha,predecessor=PREDECESSOR,predecessor_terminal_rows=terminal.splitlines(),predecessor_export_terminal=EXPORT,source_image_readback=row,exact_source_root_and_stack={k:verified['files'][k] for k in exact},runtime_profile=json.loads(profile),worker_hardware=hardware,source_requested={'vcpus':2,'memory_mib':4096,'storage_mib':10240}),indent=2)+'\n')
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root',type=Path);stage(p.parse_args().root.resolve())
