import argparse,sys,os,json,time,subprocess,threading,signal,base64,gzip
from pathlib import Path
SCRIPT=Path(__file__).resolve().parent
parser=argparse.ArgumentParser(description='Run native paused-attribution ground truth on a configured Firecracker worker.')
parser.add_argument('output',type=Path,help='New validation output directory')
parser.add_argument('--config',type=Path,required=True,help='Attribution config with matching kernel and layout paths')
parser.add_argument('--fixture',type=Path,required=True,help='Guest-compatible fixture binary compiled from fixture.c')
parser.add_argument('--driver',type=Path,default=SCRIPT.parents[1]/'bin/tsb-pages-vm')
args=parser.parse_args()
for name in ('config','fixture','driver'):
 if not getattr(args,name).is_file():parser.error('--'+name+' must name an existing file')
config=json.loads(args.config.read_text())
sys.path.insert(0,str(SCRIPT.parent/'tsb-pages'))
sys.path.insert(0,str(SCRIPT))
from run import Driver,parse_slot
from idle import IdleObserver
from paused import PausedAttributionObserver
out=args.output;out.mkdir(parents=True,exist_ok=False)
def save(n,x):(out/n).write_text(json.dumps(x,indent=2)+"\n")
events=[];tracer=None;driver=None;obs=None
try:
 daemon=int(subprocess.check_output(["systemctl","show","firecracker-containerd","--property","MainPID","--value"]))
 tracer=subprocess.Popen(["strace","-f","-e","trace=ioctl","-p",str(daemon)],stderr=subprocess.PIPE,stdout=subprocess.DEVNULL,text=True)
 def trace():
  for line in tracer.stderr:
   e=parse_slot(line)
   if e:events.append(e)
 thread=threading.Thread(target=trace);thread.start();time.sleep(1)
 driver=Driver([str(args.driver),"-output",str(out/"vm"),"-timeout","10m"],(out/"driver-stderr.log").open("w"))
 ready=driver.receive();save("ready.json",ready);assert ready["event"]=="ready"
 tracer.send_signal(signal.SIGINT);tracer.wait(timeout=10);thread.join(timeout=10);tracer=None;save("slots.json",events)
 pids={int(next(l.split()[1] for l in Path('/proc/'+str(e['tid'])+'/status').read_text().splitlines() if l.startswith('Tgid:'))) for e in events};assert len(pids)==1
 pid=pids.pop()
 config['socket_path']=ready['vm_info']['SocketPath']
 obs=PausedAttributionObserver(IdleObserver(pid,ready['vm_id'],events),config,out/'attribution')
 binary=base64.b64encode(gzip.compress(args.fixture.read_bytes())).decode()
 for i in range(0,len(binary),60000):
  setup=driver.call({'id':'stage-fixture-'+str(i),'argv':['/bin/sh','-c',"printf '%s' '"+binary[i:i+60000]+"' "+('>' if i==0 else '>>')+" /tmp/fixture.b64"],'cwd':'/','timeout_seconds':30});assert setup['exit_code']==0,setup
 setup=driver.call({'id':'unpack-fixture','argv':['/bin/sh','-c','base64 -d /tmp/fixture.b64 | gzip -d > /tmp/fixture; chmod +x /tmp/fixture'],'cwd':'/','timeout_seconds':30});assert setup['exit_code']==0,setup
 save('arm.json',obs.arm('fixture-1'))
 result=driver.call({'id':'fixture-1','argv':['/bin/sh','-c','ATTR_STRESS_MIB='+str(int(os.environ.get('ATTR_STRESS_MIB','0')))+' /tmp/fixture >/tmp/fixture-out 2>&1 & for n in $(seq 1 100); do test -s /tmp/fixture-maps && break; sleep 0.1; done; cat /tmp/fixture-out; cat /tmp/fixture-maps'],'cwd':'/','timeout_seconds':30})
 save('fixture-result.json',result);assert result['exit_code']==0
 reading=obs.collect();save('reading.json',reading)
 truth=json.loads(result['stdout'].splitlines()[0]);d=obs.decoder
 checks=[]
 def validate(event):
  d.cache={}
  tasks,vmas,owners=d.processes(set())
  matches=[t for t in tasks if t['comm']=='attr-fixture'];assert len(matches)==1,matches
  pid=matches[0]['pid'];vm=[v for v in vmas if v['pid']==pid];pgd=vm[0]['pgd_phys']
  dirs=sorted((out/'attribution').glob('step-*'));assert dirs, list((out/'attribution').iterdir())
  measured={r['gpa']:r for r in (json.loads(l) for l in gzip.open(dirs[-1]/'pages.jsonl.gz','rt'))}
  known={d.translate(truth[kind]+i*4096,pgd)&~4095 for kind,count in [('anon',8),('shared',4),('private',4),('deleted',1)] for i in range(count)}
  reference=dict(reading);reference['tool_call_id']='fixture-reference-not-access-measurement'
  reference['accessed_gpas']=[];reference['unknown_gpas']={str(g):'validation_known_reference_not_access_evidence' for g in known}
  refdir=out/'reference-attribution';refdir.mkdir()
  (refdir/'reading.json').write_text(json.dumps(reference)+'\n')
  original=d.config.get('enable_resident_sampling');d.config['enable_resident_sampling']=False
  try:d.capture(reference,refdir)
  finally:d.config['enable_resident_sampling']=original
  captured_vmas=json.loads((refdir/'vmas.json').read_text())
  rows={r['gpa']:r for r in (json.loads(l) for l in gzip.open(refdir/'pages.jsonl.gz','rt'))}
  for kind,count in [('anon',8),('shared',4),('private',4),('deleted',1)]:
   for i in range(count):
    va=truth[kind]+i*4096;g=d.translate(va,pgd)&~4095
    expected='anonymous' if kind=='anon' or (kind=='private' and i<2) else 'file_cache'
    marker=('ANON' if kind=='anon' else 'COPY' if kind=='private' and i<2 else 'DELETED' if kind=='deleted' else 'FILE')+f'-{i:02d}-unique-attribution'
    actual=d.p(g,len(marker)).decode();row=rows.get(g)
    checks.append({'kind':kind,'index':i,'gpa':g,'virtual_address':va,'expected_category':expected,'actual_category':row and row['category'],'marker_matches':actual==marker,'recorded_access':g in measured,'owner_matches':bool(row and any(captured_vmas[o['vma_index']]['pid']==pid for o in row['owners']))})
  files=json.loads((dirs[-1]/'files.json').read_text())
  fixturefiles=[f for f in files.values() if any(a.get('name_suffix','').endswith('attrfixture-disk') for a in f['aliases'])]
  assert fixturefiles,'fixture inode has no alias name'
  paths={x['path'] for f in fixturefiles for x in f['namespace_paths']}
  assert '/tmp/attrfixture-disk' in paths and '/tmp/attrfixture-link' in paths,paths
  reference_files=json.loads((refdir/'files.json').read_text())
  deletedfiles=[f for f in reference_files.values() if any(a.get('name_suffix','').endswith('attrfixture-deleted') for a in f['aliases'])]
  assert len(deletedfiles)==1 and deletedfiles[0]['deleted'],deletedfiles
  save('deleted-file-validation.json',{'passed':True,'identity':deletedfiles[0]})
  captured_tasks=json.loads((dirs[-1]/'processes.json').read_text())
  t=next(t for t in captured_tasks if t['comm']=='attr-fixture')
  assert {'attr-fixture','attr-worker'} <= {u['comm'] for u in t['thread_users']},t
  save('enhanced-validation.json',{'passed':True,'fixture_namespace_paths':sorted(paths),'fixture_thread_names':[u['comm'] for u in t['thread_users']],'fixture_file_identity':fixturefiles[0]})
  return {'checks':checks}
 obs._boundary('validation',validate)
 save('validation.json',{'checks':checks,'access_detection_all_known':all(r['recorded_access'] for r in checks),'scope':'Decoder correctness on known reference GPAs; idle tracker detection reported separately, not relabeled','passed':all(r['expected_category']==r['actual_category'] and r['marker_matches'] and r['owner_matches'] for r in checks)})
 assert all(r['expected_category']==r['actual_category'] and r['marker_matches'] and r['owner_matches'] for r in checks),checks
except BaseException as e:
 save('failure.json',{'type':type(e).__name__,'message':str(e)});raise
finally:
 if obs:obs.close()
 if tracer:tracer.send_signal(signal.SIGINT);tracer.wait(timeout=10)
 if driver:save('driver-exit.json',{'code':driver.close()})
