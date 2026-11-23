"""Match actual v1/v4 stacks and publish unique log-summary reservations on amd201."""
import argparse
import datetime
import hashlib
import json
from pathlib import Path
import runpy
import subprocess

HERE=Path(__file__).resolve().parent
q=runpy.run_path(str(HERE/'queue.py'))
sha=q['digest']; NODE='amd201'; STAGE=q['STAGE']
CC='/local/zb-relaxed-compcert-01'
TRIAL='1d11a94f-d715-44d6-9ff6-1f097a8793f8'
def call(node,*args,**k):return q['ssh'](node,q['privileged'](*args),**k)
def output(node,*args):return call(node,*args,capture_output=True,text=True).stdout.strip()
def runtime(root,level,variant):
    if level=='deep':
        x=json.loads((root/f'plan/result/1_opportunity/deep-sweep/compcert-{variant}.json').read_bytes())
        return x['deployed_files'],x['runtime_profile']
    node='amd182'
    previous=('/local/zb-validation/runtime' if variant=='v1' else '/local/zb-relaxed-compcert-v4-runtime/runtime')
    pre=json.loads(output(node,'cat',f'{CC}/{variant}/{TRIAL}/idle-before.json'))
    return pre['files'],json.loads(output(node,'cat',previous+'/runtime.json'))
def main(root):
    cache=root/'repo/ARIES/.cache';raw=cache/'zb-raw/relaxed-compcert-deep/v4'
    assert (raw/'worker.sha256').is_file() and sha(raw/'worker.tar.gz')==(raw/'worker.sha256').read_text().split()[0]
    assert q['status'](NODE,'zb-relaxed-compcert-deep-v4-01')=='inactive'
    dst=root/'plan/result/1_opportunity/replacements/log-summary-date-ranges/relocated-modes.json'
    assert not dst.exists()
    source=json.loads((dst.parent/'sources.json').read_bytes())
    bundle=cache/'harbor/log-summary-replacement/stage.tar.gz';assert sha(bundle)==q['PAYLOAD']
    call(NODE,'mkdir','-p',STAGE)
    with bundle.open('rb') as f:call(NODE,'tar','-C',STAGE,'-xzf','-',stdin=f)
    call(NODE,'bash','-c','cd "$1" && sha256sum --quiet -c inputs.sha256','sh',STAGE)
    blob=(HERE/'run-series.sh').read_bytes();old=b'^amd(182|189)$';assert blob.count(old)==1
    assert hashlib.sha256(blob).hexdigest()=='73cf4d98ab58a788ef3b4b86daf6b941279438545eac31b0a7f37eef460ce6e2'
    amended=blob.replace(old,b'^amd201$');driver=STAGE+'/run-series-amd201.sh'
    call(NODE,'tee',driver,input=amended,stdout=subprocess.DEVNULL);call(NODE,'chmod','0755',driver)
    amended_sha=hashlib.sha256(amended).hexdigest();assert output(NODE,'sha256sum',driver).split()[0]==amended_sha
    image='docker.io/alexgshaw/log-summary-date-ranges@sha256:cbeb6ba905c2fec294f16cd5e16e3ea7f2e04d38ac2484d51a11de262aa7dc51'
    modes=[];exact=['rootfs','kernel','firecracker','daemon','live_daemon','shim','ctr','runc','jailer','jailer_config']
    for level,v,unit,rt in q['SERIES'][NODE]:
        oldnode=next(n for n in ('amd182','amd189') if any((l,w)==(level,v) for l,w,*_ in q['SERIES'][n]))
        assert q['status'](oldnode,f'zb-log-summary-{oldnode}-{level}-{v}')=='inactive'
        call(oldnode,'test','!','-e',STAGE+'/'+level+'-'+v)
        donor,old_profile=runtime(root,level,v)
        audit=json.loads(output(NODE,'python3','-B',STAGE+'/audit-idle-runtime.py',unit,rt));actual=audit['files']
        assert all(actual[k]['sha256']==donor[k]['sha256'] for k in exact),(level,v,'deployed inputs')
        for phase in ('before','after'):
            assert not audit[phase]['vmm_processes'] and not any(audit[phase]['tasks_by_socket'].values())
        oldrt=str(Path(old_profile['root_drive']).parent)
        profile=output(NODE,'cat',rt+'/runtime.json')
        assert json.loads(profile.replace(rt,oldrt))==old_profile,(level,v,'profile')
        ctr=[rt+'/bin/firecracker-ctr','-a','/run/'+unit+'/containerd.sock','-n','aries']
        if image not in output(NODE,*ctr,'images','ls','-q').splitlines():
            call(NODE,*ctr,'images','pull','--local','--snapshotter','devmapper','--platform','linux/amd64',image)
        row,=[s for s in output(NODE,*ctr,'images','ls').splitlines() if s.split() and s.split()[0]==image]
        assert row.count(image.split('@')[1])>=2
        modes.append(dict(level=level,variant=v,previous_node=oldnode,source_sha256={k:actual[k]['sha256'] for k in exact},runtime=rt,service=unit,profile=json.loads(profile),worker=audit['node'],source_image_readback=row))
    phy=json.loads((root/'plan/result/1_opportunity/relaxed-compcert/deep/relocated-v4.json').read_bytes())['worker_hardware']
    assert output(NODE,'cat','/proc/sys/kernel/random/boot_id')==phy['boot_id']
    receipt=dict(active_node=NODE,utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),profiles=modes,worker_hardware=phy,
        worker_stage_sha256=q['PAYLOAD'],adapter_sha256=source['adapter_sha256'],original_launcher_sha256=hashlib.sha256(blob).hexdigest(),amd201_launcher_sha256=amended_sha,
        launcher_difference='Only machine-name allowlist at worker placement preflight.',deep_median=source['selected_trial'],
        worker_source_lock=source['worker_stage_manifest_sha256'],past_compcert_v4_archive_sha256=sha(raw/'worker.tar.gz'))
    tmp=dst.with_suffix('.partial');tmp.write_text(json.dumps(receipt,indent=2)+'\n');tmp.replace(dst)
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root',type=Path);main(p.parse_args().root.resolve())
