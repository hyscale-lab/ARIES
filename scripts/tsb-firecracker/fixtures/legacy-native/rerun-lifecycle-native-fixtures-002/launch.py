#!/usr/bin/env python3
"""Exactly two no-cost mock fixtures; restore runtime and retain all evidence."""
import hashlib,json,os,subprocess,sys,time,signal,http.client,socket
from pathlib import Path
ROOT=Path(__file__).resolve().parent
REPO=Path("/home/nehalem/aries")
os.umask(0o077);os.chdir(REPO)
sys.path.insert(0,str(REPO/"scripts/tsb-pages"))
import suite
from vmm_observer import Connection,process_start
def save(path,data):suite.atomic_json(path,data)
def sha(data):return hashlib.sha256(data).hexdigest()
runtime=Path("/etc/containerd/firecracker-runtime.json")
original=runtime.read_bytes()
save(ROOT/"runtime-original.json",json.loads(original))
(ROOT/"runtime-original.bytes").write_bytes(original)
pins=json.loads((ROOT/"preparation.json").read_text())
assert sha(Path(pins["candidate_binary"]).read_bytes())==pins["candidate_sha256"]
before=suite.inventory();save(ROOT/"before-inventory.json",before)
assert before["clean"],"worker not clean before no-cost gate"
for case in ["normal","paused-deadline"]:
 root=ROOT/case
 selected=(root/"runtime-selected.json").read_bytes()
 events=(root/"host-monitor.jsonl").open("x")
 assert runtime.read_bytes()==original,"runtime baseline changed before fixture"
 save(root/"before-inventory.json",suite.inventory())
 process=None;code=None
 try:
  runtime.write_bytes(selected)
  with (root/"driver.log").open("x") as log:
   process=subprocess.Popen([sys.executable,str(root/"run-fixture.py")],stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
   save(root/"launch.json",{"driver_pid":process.pid,"begin_ns":time.monotonic_ns(),"wall_ns":time.time_ns(),"paid_requests":0,"candidate_sha256":pins["candidate_sha256"]})
   deadline=time.monotonic()+620
   while process.poll() is None:
    row={"monotonic_ns":time.monotonic_ns(),"wall_ns":time.time_ns()}
    vmm=root/"observer/vmm.json"
    if vmm.exists():
     identity=json.loads(vmm.read_text())
     try:
      pid=identity["pid"];start=identity["start_ticks"]
      if process_start(pid)!=start:raise RuntimeError("VMM start identity changed")
      conn=Connection("/proc/"+str(pid)+"/cwd/firecracker.sock",1)
      try:
       conn.request("GET","/");response=conn.getresponse()
       row["vmm_http_status"]=response.status;row["vmm_state"]=json.loads(response.read(16384)).get("state")
      finally:conn.close()
     except (OSError,ValueError,http.client.HTTPException,RuntimeError) as error:row["vmm_state_unavailable"]=type(error).__name__
    paths=list((root/"runs").glob("*/fix-git-001/evaluation"))
    row["evaluation_directory_exists"]=bool(paths)
    events.write(json.dumps(row)+"\n");events.flush()
    if time.monotonic()>deadline:raise TimeoutError("bounded native mock fixture did not close")
    time.sleep(1)
   code=process.returncode
 finally:
  events.close()
  after=suite.inventory()
  restored=False
  if process is not None and process.poll() is None:
   save(root/"unfinished-driver.json",{"pid":process.pid,"reason":"no automatic destructive cleanup","inventory":after})
  elif runtime.read_bytes()==selected:
   runtime.write_bytes(original);restored=runtime.read_bytes()==original
  save(root/"launch-finish.json",{"driver_returncode":code,"wall_ns":time.time_ns(),"runtime_restored":restored,"inventory":after})
 if code!=0 or not restored or not after["clean"]:raise RuntimeError("fixture failed; retained original evidence")
save(ROOT/"completion.json",{"paid_requests":0,"cases":["normal","paused-deadline"],"runtime_restored":runtime.read_bytes()==original,"inventory":suite.inventory()})
