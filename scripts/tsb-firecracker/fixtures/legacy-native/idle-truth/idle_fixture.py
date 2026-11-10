#!/usr/bin/env python3
import gzip,sys
from idle import IdleObserver
import argparse,base64,hashlib,http.client,json,os,pathlib,re,socket,subprocess,time
ROOT=pathlib.Path('/home/nehalem/aries')
BASE=ROOT/'runs/tsb-access-dirty-001/idle-truth'
BIN=ROOT/'.cache/tsb-access-dirty-001/firecracker/build/cargo_target/x86_64-unknown-linux-musl/release/firecracker'
class Conn(http.client.HTTPConnection):
 def __init__(self,path):super().__init__('localhost',timeout=30);self.path=path
 def connect(self):self.sock=socket.socket(socket.AF_UNIX);self.sock.connect(str(self.path))
def run(out,mem_mib):
 out.mkdir(); sock=out/'api.sock'; vs=out/'vs.sock'; tap='adirty218'
 def call(method,path,body=None):
  c=Conn(sock);c.request(method,path,json.dumps(body) if body is not None else None,{'Content-Type':'application/json'});r=c.getresponse();raw=r.read();c.close();d=json.loads(raw) if raw else None
  calls.append({'method':method,'path':path,'body':body,'status':r.status,'response':d})
  if r.status>=400:raise RuntimeError((path,r.status,d))
  return d
 def wait_text(pattern,seconds=40):
  end=time.time()+seconds
  while time.time()<end:
   s=(out/'serial.log').read_text(errors='replace');m=re.search(pattern,s)
   if m:return m
   if proc.poll() is not None:raise RuntimeError('FC exited '+str(proc.returncode)+' '+s[-2000:])
   time.sleep(.1)
  raise RuntimeError('missing serial '+pattern+' '+s[-2000:])
 def dirty_pages(reply,key):
  pages=set()
  for r in reply['regions']:
   b=base64.b64decode(r[key]);base=r['guest_base']//4096
   for i in range(r['page_count']):
    if b[i//8]>>(i%8)&1:pages.add(base+i)
  return pages
 def marker_gpas(reply,marker):
  maps=[]
  for line in pathlib.Path(f'/proc/{proc.pid}/maps').read_text().splitlines():
   a,b=(int(x,16) for x in line.split()[0].split('-'))
   if 'rw' in line.split()[1]:maps.append((a,b))
  fd=os.open(f'/proc/{proc.pid}/mem',os.O_RDONLY);found=[]
  try:
   for r in reply['regions']:
    candidates=[a for a,b in maps if b-a==r['bytes']]
    assert len(candidates)==1,('RAM mapping ambiguous',r['guest_base'],r['bytes'],candidates)
    hva=candidates[0];base=r['guest_base']//4096
    for page in dirty_pages(reply,'vmm_dirty_b64'):
     offset=(page-base)*4096
     if 0<=offset<r['bytes'] and marker in os.pread(fd,4096,hva+offset):found.append(page)
  finally:os.close(fd)
  return found
 calls=[];results={};proc=None;conn=None;udp=None;tap_created=False
 try:
  subprocess.run(['ip','tuntap','add','dev',tap,'mode','tap'],check=True);tap_created=True
  subprocess.run(['ip','addr','add','10.231.218.1/24','dev',tap],check=True)
  subprocess.run(['ip','link','set',tap,'up'],check=True)
  subprocess.run(['ip','neigh','replace','10.231.218.2','lladdr','06:00:ac:10:00:02','dev',tap],check=True)
  disk=out/'block.bin'
  with disk.open('wb') as f:f.write(bytes([167])*4096);f.truncate(8*1024*1024)
  env=dict(os.environ,ARIES_DIRTY_LOG='1')
  serial=(out/'serial.log').open('wb')
  proc=subprocess.Popen([str(BIN),'--api-sock',str(sock),'--id','aries-dirty-fixture'],stdin=subprocess.PIPE,stdout=serial,stderr=subprocess.STDOUT,env=env)
  for _ in range(100):
   if sock.exists():break
   time.sleep(.05)
  (out/'metrics.log').touch();call('PUT','/metrics',{'metrics_path':str(out/'metrics.log')})
  call('PUT','/machine-config',{'vcpu_count':1,'mem_size_mib':mem_mib})
  config=call('GET','/machine-config');assert config['track_dirty_pages'] is True
  call('PUT','/boot-source',{'kernel_image_path':'/var/lib/firecracker-containerd/runtime/vmlinux-5.10.239','initrd_path':str(BASE/'fixture-initrd.cpio'),'boot_args':'console=ttyS0 reboot=k panic=1 pci=off rdinit=/init'})
  call('PUT','/drives/blk',{'drive_id':'blk','path_on_host':str(disk),'is_root_device':False,'is_read_only':True,'io_engine':'Async'})
  call('PUT','/network-interfaces/net',{'iface_id':'net','host_dev_name':tap,'guest_mac':'06:00:ac:10:00:02'})
  call('PUT','/vsock',{'guest_cid':218,'uds_path':str(vs)})
  call('PUT','/actions',{'action_type':'InstanceStart'})
  m=wait_text(r'PFNS (\d+) (\d+) (\d+) (\d+)');pfns=list(map(int,m.groups()));results['pfns']=pfns
  control=int(wait_text(r'CONTROL_PFN (\d+)').group(1));results['untouched_control_pfn']=control
  wait_text('VSLISTEN')
  conn=socket.socket(socket.AF_UNIX);conn.settimeout(10);conn.connect(str(vs));conn.sendall(b'CONNECT 5000\n');results['vsock_connect']=conn.recv(128).decode()
  wait_text('VSREADY')
  call('PATCH','/vm',{'state':'Paused'})
  arm=call('PUT','/dirty-log',{'operation':'arm','epoch':1})
  maps=[]
  for line in pathlib.Path(f'/proc/{proc.pid}/maps').read_text().splitlines():
   lo,hi=(int(x,16) for x in line.split()[0].split('-'))
   if 'rw' in line.split()[1] and hi-lo==256*1024*1024:maps.append(lo)
  assert len(maps)==1
  slots=[{'guest_phys_addr':0,'userspace_addr':maps[0],'memory_size':256*1024*1024}]
  observer=IdleObserver(proc.pid,'idle-truth-fixture',slots)
  results['slots']=slots;results['trials']=[];seq=0;epoch=1
  def execute():
   nonlocal seq
   seq+=1
   call('PATCH','/vm',{'state':'Resumed'});proc.stdin.write(b'markers\n');proc.stdin.flush()
   wait_text(r'MARKERS_DONE seq='+str(seq)+r' read=0 restore=0 block=4096 first=167')
   call('PATCH','/vm',{'state':'Paused'})
  try:
   for trial in range(12):
    # Warm translations before every trial; no observer payload reads.
    execute()
    reset_before_arm=trial<4 or trial>=8
    if reset_before_arm:
     call('PUT','/dirty-log',{'operation':'collect','epoch':epoch,'next_epoch':epoch+1});epoch+=1
    arm_receipt=observer.arm('controlled-'+str(trial))
    before={str(p*4096):observer.armed['before'].get(p*4096) for p in pfns+[control]}
    execute()
    access=observer.collect()
    dirty=call('PUT','/dirty-log',{'operation':'collect','epoch':epoch,'next_epoch':epoch+1});epoch+=1
    with gzip.open(out/f'access-{trial:02d}.json.gz','wt') as f:json.dump(access,f)
    (out/f'dirty-{trial:02d}.json').write_text(json.dumps(dirty))
    aset=set(access['accessed_gpas']);iset=set(access['idle_gpas']);cpuset=dirty_pages(dirty,'kvm_dirty_b64');vset=dirty_pages(dirty,'vmm_dirty_b64')
    truth=[]
    for p,kind in zip(pfns+[control],['read-only','write','write-restore','direct-block-read-destination','untouched-control']):
     gpa=p*4096
     truth.append({'gpa':gpa,'operation':kind,'raw_access':'observed' if gpa in aset else 'idle' if gpa in iset else 'unknown' if str(gpa) in access['unknown_gpas'] else 'absent','unknown_reason':access['unknown_gpas'].get(str(gpa)),'arm_mapping':before[str(gpa)],'kvm_dirty':p in cpuset,'vmm_dirty':p in vset})
    results['trials'].append({'trial':trial,'reset_immediately_before_access_arm':reset_before_arm,'dirty_window_includes_warmup':not reset_before_arm,'arm_receipt':arm_receipt,'truth':truth,'access_counts':{'observed':len(aset),'idle':len(iset),'unknown':len(access['unknown_gpas'])}})
   results['read_only_raw_idle_misses']=sum(x['raw_access']=='idle' for t in results['trials'] for x in t['truth'] if x['operation']=='read-only')
   results['written_raw_idle_misses']=sum(x['raw_access']=='idle' for t in results['trials'] for x in t['truth'] if x['operation'] in ['write','write-restore'])
   results['control_classifications']=[x['raw_access'] for t in results['trials'] for x in t['truth'] if x['operation']=='untouched-control']
   results['completed']=True
   results['passed_no_known_raw_idle_misses']=not(results['read_only_raw_idle_misses'] or results['written_raw_idle_misses'])
  finally:observer.close()
 except Exception as e:results['passed']=False;results['error']=repr(e);raise
 finally:
  if conn:conn.close()
  if udp:udp.close()
  if proc and proc.poll() is None:proc.terminate();proc.wait(timeout=20)
  if tap_created:subprocess.run(['ip','link','delete',tap],check=True)
  results['cleanup']={'process_exited':proc is None or proc.poll() is not None,'tap_removed':tap_created}
  (out/'api-calls.json').write_text(json.dumps(calls,indent=2)+'\n')
  (out/'result.json').write_text(json.dumps(results,indent=2)+'\n')
if __name__=='__main__':
 p=argparse.ArgumentParser();p.add_argument('--out',required=True);p.add_argument('--mem-mib',type=int,default=256);a=p.parse_args();run(pathlib.Path(a.out),a.mem_mib)

