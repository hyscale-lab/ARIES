#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Disposable real-VM companion gate; no paid model and no external process-memory access."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from vmm_api import VmmAPI,UnixConnection,ack_record,finalize_capture
from epochs import EpochCoordinator
from process_adapter import ExternalCapture

def save(path,value):Path(path).write_text(json.dumps(value,indent=2)+"\n")
def rows(path):
 with gzip.open(path,"rt") as stream:return [json.loads(line) for line in stream if line.strip()]

def run(args):
 out=args.out.resolve();out.mkdir(parents=True,exist_ok=False);sock=out/"api.sock"
 companion=Path(__file__).resolve().parents[1]
 serial=(out/"serial.log").open("wb")
 disk=out/"block.bin"
 with disk.open("wb") as stream:stream.write(bytes([167])*4096);stream.truncate(8<<20)
 proc=None;children=[];calls=[];result={"passed":False,"paid_requests":0,
  "binary_sha256":hashlib.sha256(args.binary.read_bytes()).hexdigest(),
  "kernel_sha256":hashlib.sha256(args.kernel.read_bytes()).hexdigest(),
  "initrd_sha256":hashlib.sha256(args.initrd.read_bytes()).hexdigest(),
  "config_sha256":hashlib.sha256(args.config.read_bytes()).hexdigest(),
  "companion_source_sha256":{str(p.relative_to(companion)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(companion.rglob("*")) if p.is_file() and "__pycache__" not in p.parts}}
 def call(method,path,body):
  c=UnixConnection(sock,30)
  try:
   c.request(method,path,json.dumps(body),{"Content-Type":"application/json"})
   response=c.getresponse();data=response.read();value=json.loads(data) if data else None
   calls.append({"method":method,"path":path,"body":body,"status":response.status,"response":value})
   if response.status not in (200,204):raise RuntimeError((path,response.status,value))
   return value
  finally:c.close()
 def wait(pattern):
  deadline=time.monotonic()+40
  while time.monotonic()<deadline:
   match=re.search(pattern,(out/"serial.log").read_text(errors="replace"))
   if match:return match
   if proc.poll() is not None:raise RuntimeError("VMM exited "+str(proc.returncode))
   time.sleep(.05)
  raise TimeoutError("guest marker missing "+pattern)
 def child(capture_id,epoch,directory,**kwargs):
  c=ExternalCapture(companion,api,sock,capture_id,coordinator.generation,epoch,out/directory,timeout_seconds=120)
  children.append(c);c.start(**kwargs);seal=c.await_sealed();return c,seal
 def finish(c,seal,rotation):
  c.await_publication(timeout=120)
  begin=json.loads((c.output/"capture-begin.json").read_text())
  info=finalize_capture(api,begin,seal);save(c.output/"finalized-info.json",info)
  assert info["consistency"]=="valid",info
  validation=next(v for v in rotation["capture_validation"] if v["capture_id"]==c.capture_id)
  assert validation["consistency"]=="valid",validation
  save(c.output/"validation.json",validation)
  for record in dict.fromkeys((seal["capture_record_id"],info["capture_record_id"])):
   ack_record(api,"/metadata-capture",record,coordinator.generation)
  ack_record(api,"/memory-census",begin["census_record_id"],coordinator.generation)
  return info
 try:
  proc=subprocess.Popen([str(args.binary.resolve()),"--api-sock",str(sock),"--id","aries-companion-fixture"],
    stdin=subprocess.PIPE,stdout=serial,stderr=subprocess.STDOUT,env={**os.environ,"ARIES_DIRTY_LOG":"1"})
  for _ in range(200):
   if sock.exists():break
   if proc.poll() is not None:raise RuntimeError("VMM launch failed")
   time.sleep(.05)
  call("PUT","/machine-config",{"vcpu_count":1,"mem_size_mib":256})
  call("PUT","/boot-source",{"kernel_image_path":str(args.kernel.resolve()),"initrd_path":str(args.initrd.resolve()),
       "boot_args":"console=ttyS0 reboot=k panic=1 pci=off rdinit=/init"})
  call("PUT","/drives/blk",{"drive_id":"blk","path_on_host":str(disk),"is_root_device":False,"is_read_only":True,"io_engine":"Async"})
  call("PUT","/actions",{"action_type":"InstanceStart"})
  match=wait(r"READY pid=(\d+) file_pfn=(\d+) anon_pfns=(\d+),(\d+),(\d+),(\d+) first=75")
  known=list(map(int,match.groups()));result["guest_known_objects"]={"pid":known[0],"file_gpa":known[1]*4096,"anonymous_gpas":[p*4096 for p in known[2:]],"comm":"aries-attrib","path":"/known-file"}
  call("PATCH","/vm",{"state":"Paused"})
  api=VmmAPI(sock,timeout=30);coordinator=EpochCoordinator(api,["dirty","idle","census","metadata"])
  arm=coordinator.arm("bootstrap");save(out/"arm.json",arm);coordinator.publish(arm,out/"epoch0-baseline")
  boot,bootseal=child("bootstrap-1",1,"bootstrap",config=args.config.resolve(),vm_id="aries-companion-fixture")
  bootrotate=coordinator.rotate("llm_wait");coordinator.publish(bootrotate,out/"epoch1-bootstrap")
  finish(boot,bootseal,bootrotate)
  bootstrap=json.loads((boot.output/"bootstrap.json").read_text())
  initialgap=coordinator.rotate("tool");coordinator.publish(initialgap,out/"epoch2-llm-wait")
  call("PATCH","/vm",{"state":"Resumed"})
  proc.stdin.write(b"touch\n");proc.stdin.flush()
  end=wait(r"DONE pid=(\d+) file_pfn=(\d+) file=75 read=0 restored=0 block=4096")
  assert int(end[1])==known[0] and int(end[2])==known[1],"known guest object identity changed"
  call("PATCH","/vm",{"state":"Paused"})
  tool=coordinator.rotate("llm_wait");coordinator.publish(tool,out/"epoch3-tool")
  access=rows(out/"epoch3-tool/access.jsonl.gz");dirty=rows(out/"epoch3-tool/dirty.jsonl.gz")
  observed=[r["gpa"] for r in access if r["state"]=="observed"]
  unknown={str(r["gpa"]):r["unknown_reason"] for r in access if r["state"]=="unknown"}
  additional={str(r["gpa"]):[label for key,label in (("kvm_dirty","kvm_dirty"),("vmm_dirty","vmm_potential_write")) if r[key]] for r in dirty}
  reading={"schema":"aries.companion.fixture-footprint.v1","page_bytes":4096,"vm_instance":"aries-companion-fixture",
    "vmm_pid":proc.pid,"tool_call_id":"known-objects","vm_generation":coordinator.generation,
    "regions":coordinator.regions,"layout_identity":bootstrap["capture_binding"]["layout_identity"],
    "source_epoch":3,"source_record_id":tool["epoch_record_id"],"capture_epoch":4,
    "accessed_gpas":observed,"unknown_gpas":unknown,
    "additional_gpas":[int(g) for g in additional],"additional_access_sources":additional}
  assert known[1]*4096 in set(observed)|set(map(int,unknown))|set(map(int,additional)),"known file read missing from measured set"
  save(out/"reading.json",reading)
  capture,seal=child("attribution-1",4,"capture",bootstrap=boot.output/"bootstrap.json",reading=out/"reading.json",
       source_epoch=3,source_record_id=tool["epoch_record_id"])
  final=coordinator.rotate(close=True);coordinator.publish(final,out/"epoch4-llm-wait")
  call("PATCH","/vm",{"state":"Resumed"})
  finish(capture,seal,final)
  page_rows=rows(capture.output/"attribution/pages.jsonl.gz");by={r["gpa"]:r for r in page_rows}
  file_row=by[known[1]*4096];files=json.loads((capture.output/"attribution/files.json").read_text())
  paths=files[file_row["file_ref"]]["namespace_paths"]
  assert any(p["path"]=="/known-file" for p in paths),paths
  assert "aries-attrib" in file_row["process_names"],file_row
  assert any(owner["pid"]==known[0] for owner in file_row["process_owners"]),file_row
  anon_row=by[known[3]*4096]
  assert anon_row["category"]=="anonymous" and "aries-attrib" in anon_row["process_names"],anon_row
  result["known_file_row"]=file_row;result["known_file_paths"]=paths;result["known_anon_row"]=anon_row
  # replay.py expects reading beside attribution; retained input is immutable.
  (capture.output/"attribution/reading.json").write_bytes((capture.output/"reading.json").read_bytes())
  replay=subprocess.run([sys.executable,str(companion/"replay.py"),"--step",str(capture.output/"attribution"),
    "--bootstrap",str(boot.output/"bootstrap.json"),"--output",str(out/"replay")],
    stdout=(out/"replay.log").open("w"),stderr=subprocess.STDOUT,timeout=180)
  assert replay.returncode==0,(out/"replay.log").read_text()
  result["replay_exact"]=True;result["passed"]=True
 except BaseException as error:
  result["error"]={"type":type(error).__name__,"message":str(error)}
  raise
 finally:
  for c in children:
   if c.process is not None and c.process.poll() is None:
    try:c.abort("fixture cleanup")
    except Exception as error:result.setdefault("child_cleanup_errors",[]).append(str(error))
  if proc is not None and proc.poll() is None:
   proc.terminate()
   try:proc.wait(timeout=20)
   except subprocess.TimeoutExpired:proc.kill();proc.wait(timeout=5)
  serial.close();result["cleanup"]={"vmm_exited":proc is None or proc.poll() is not None,"no_tap_created":True}
  save(out/"control-calls.json",calls)
  if "api" in locals():save(out/"api-receipts.json",api.calls)
  save(out/"result.json",result)

if __name__=="__main__":
 p=argparse.ArgumentParser(description=__doc__)
 for name in ("out","binary","kernel","initrd","config"):p.add_argument("--"+name,required=True,type=Path)
 run(p.parse_args())
