"""Actual server dispatch with a deterministic VMM seam; no guest or model."""
import concurrent.futures
import http.client
import json
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import unittest

FAKE = r"""
import json,threading,time
from pathlib import Path
class VmmObserver:
 def __init__(self,*args):
  self.start=1;self.abort=threading.Event();self.lock=threading.Lock()
  self.interrupts=0;self.releases=0;self.cleanup_started=threading.Event()
  self.output=Path(args[-1]);self.output.mkdir(parents=True)
 def session_start(self):
  self.abort.wait(30)
  if self.abort.is_set():raise RuntimeError("initialization canceled")
  return {"ok":True}
 def arm(self,batch):return {"arm_end_ns":time.monotonic_ns()}
 def collect(self):return {"schema":"fixture","batch_uncertainty":[]}
 def request_abort(self):
  with self.lock:
   if self.abort.is_set():return
   self.abort.set();self.interrupts+=1
 def abort_session(self,reason):
  self.cleanup_started.set();time.sleep(.08)
  self.releases+=1
  return {"ok":True,"released":True,"paused":False,"measurement_valid":False,
          "terminal_complete":False,"interrupts":self.interrupts,"release_attempt":self.releases}
 def close(self):
  (self.output/"close.json").write_text(json.dumps({"releases":self.releases,"abort_requested":self.abort.is_set()}))
def discover(prefix):return (1,prefix+"fixture",1,"unused")
"""

class HTTPTests(unittest.TestCase):
 def request(self,port,path,body):
  conn=http.client.HTTPConnection("127.0.0.1",port,timeout=3)
  try:
   conn.request("POST",path,json.dumps(body),{"Content-Type":"application/json"})
   response=conn.getresponse()
   return response.status,json.loads(response.read())
  finally:conn.close()
 def launch(self,root,fake=FAKE):
  for name in ("serve.py","batches.py"):shutil.copyfile(Path(__file__).with_name(name),root/name)
  (root/"vmm_observer.py").write_text(fake)
  (root/"config.json").write_text('{"backend":"vmm_owned"}')
  sock=socket.socket();sock.bind(("127.0.0.1",0));port=sock.getsockname()[1];sock.close()
  log=(root/"server.log").open("w")
  try:
   process=subprocess.Popen([sys.executable,str(root/"serve.py"),"--output",str(root/"observer"),
      "--port",str(port),"--access-dirty-config",str(root/"config.json")],stdout=log,stderr=log)
  finally:log.close()
  deadline=time.monotonic()+5
  while not (root/"observer/ready.json").exists():
   if time.monotonic()>deadline or process.poll() is not None:
    process.kill();process.wait();self.fail("server did not start")
   time.sleep(.01)
  return process,port
 def finish(self,process):
  if process.poll() is None:process.terminate()
  try:process.wait(timeout=3)
  except subprocess.TimeoutExpired:process.kill();process.wait(timeout=3)
 def test_signal_interrupts_active_boundary_and_confirms_release(self):
  with tempfile.TemporaryDirectory() as temp:
   root=Path(temp);process,port=self.launch(root)
   try:
    with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
     request=pool.submit(self.request,port,"/session/start",{"session_id":"s","turn_id":"t"})
     deadline=time.monotonic()+2
     while not (root/"observer/vmm.json").exists():
      if time.monotonic()>deadline:self.fail("request did not acquire observer")
      time.sleep(.005)
     process.terminate()
     process.wait(timeout=2)
     self.assertEqual(request.result(timeout=2)[0],409)
    release=json.loads((root/"observer/access-dirty/close.json").read_text())
    self.assertEqual(release,{"releases":1,"abort_requested":True})
    self.assertEqual(process.returncode,0)
   finally:self.finish(process)
 def test_successful_boundaries_echo_the_bound_native_session(self):
  with tempfile.TemporaryDirectory() as temp:
   root=Path(temp);process,port=self.launch(root,FAKE.replace("self.abort.wait(30)","pass"))
   identity={"session_id":"s","turn_id":"t"}
   declaration={**identity,"batch_id":"b","llm_step_id":"b","expected_tool_call_ids":["a"],
      "requested_tools":[{"tool_call_id":"a","tool_name":"exec"}]}
   member={**identity,"batch_id":"b","llm_step_id":"b","tool_call_id":"a","tool_name":"exec"}
   try:
    for path,body in [("/session/start",identity),("/batch",declaration),("/begin",member),("/end",member)]:
     status,ack=self.request(port,path,body)
     self.assertEqual(status,200,ack)
     self.assertEqual({name:ack.get(name) for name in identity},identity)
   finally:self.finish(process)
 def test_batch_cannot_initialize_a_session_without_pause_barrier(self):
  with tempfile.TemporaryDirectory() as temp:
   root=Path(temp);process,port=self.launch(root)
   declaration={"session_id":"s","turn_id":"t","batch_id":"b","llm_step_id":"b",
      "expected_tool_call_ids":["a"],"requested_tools":[{"tool_call_id":"a","tool_name":"exec"}]}
   try:
    status,ack=self.request(port,"/batch",declaration)
    self.assertEqual(status,409)
    self.assertFalse(ack["ok"])
    self.assertIn("no initialized VMM-owned session",ack["error"])
    self.assertFalse((root/"observer/vmm.json").exists())
    self.assertFalse((root/"observer/access-dirty").exists())
   finally:self.finish(process)
 def test_failed_shutdown_release_still_closes_observer_and_reports_failure(self):
  with tempfile.TemporaryDirectory() as temp:
   root=Path(temp)
   fake=FAKE.replace("self.abort.wait(30)","pass").replace(
      "self.cleanup_started.set();time.sleep(.08)","raise RuntimeError('release failed')")
   process,port=self.launch(root,fake)
   try:
    self.assertEqual(self.request(port,"/session/start",{"session_id":"s","turn_id":"t"})[0],200)
    process.terminate();process.wait(timeout=2)
    self.assertNotEqual(process.returncode,0)
    closed=json.loads((root/"observer/access-dirty/close.json").read_text())
    self.assertEqual(closed,{"releases":0,"abort_requested":True})
    errors=[json.loads(line) for line in (root/"observer/errors.jsonl").read_text().splitlines()]
    self.assertEqual(errors[-1]["error"],"observer shutdown failed")
    self.assertIn("release failed",errors[-1]["message"])
   finally:self.finish(process)
 def test_repeated_signal_during_release_cannot_reenter_observer_lock(self):
  with tempfile.TemporaryDirectory() as temp:
   root=Path(temp)
   fake=FAKE.replace("self.abort.wait(30)","pass").replace(
      "self.cleanup_started.set();time.sleep(.08)",
      "with self.lock:\n   import os,signal\n   os.kill(os.getpid(),signal.SIGTERM)\n   time.sleep(.08)")
   process,port=self.launch(root,fake)
   try:
    self.assertEqual(self.request(port,"/session/start",{"session_id":"s","turn_id":"t"})[0],200)
    process.terminate();process.wait(timeout=2)
    closed=json.loads((root/"observer/access-dirty/close.json").read_text())
    self.assertEqual(closed,{"releases":1,"abort_requested":True})
    self.assertEqual(process.returncode,0)
   finally:self.finish(process)
 def test_abort_preempts_request_and_duplicate_does_not_interrupt_cleanup(self):
  with tempfile.TemporaryDirectory() as temp:
   root=Path(temp)
   for name in ("serve.py","batches.py"):shutil.copyfile(Path(__file__).with_name(name),root/name)
   (root/"vmm_observer.py").write_text(FAKE)
   (root/"config.json").write_text('{"backend":"vmm_owned"}')
   sock=socket.socket();sock.bind(("127.0.0.1",0));port=sock.getsockname()[1];sock.close()
   with (root/"server.log").open("w") as log:
    process=subprocess.Popen([sys.executable,str(root/"serve.py"),"--output",str(root/"observer"),
       "--port",str(port),"--access-dirty-config",str(root/"config.json")],stdout=log,stderr=log)
    try:
     deadline=time.monotonic()+5
     while not (root/"observer/ready.json").exists():
      if time.monotonic()>deadline or process.poll() is not None:self.fail("server did not start")
      time.sleep(.01)
     identity={"session_id":"session","turn_id":"1"}
     with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
      normal=pool.submit(self.request,port,"/session/start",identity)
      deadline=time.monotonic()+2
      while not (root/"observer/vmm.json").exists():
       if time.monotonic()>deadline:self.fail("normal request not initialized")
       time.sleep(.005)
      wrong_status,_=self.request(port,"/session/abort",{**identity,"session_id":"wrong"})
      self.assertEqual(wrong_status,409)
      first=pool.submit(self.request,port,"/session/abort",identity)
      time.sleep(.01)
      duplicate=pool.submit(self.request,port,"/session/abort",identity)
      self.assertEqual(normal.result(timeout=3)[0],409)
      for future in (first,duplicate):
       status,ack=future.result(timeout=3)
       self.assertEqual(status,200);self.assertTrue(ack["released"])
       self.assertEqual(ack["interrupts"],1);self.assertFalse(ack["measurement_valid"])
     status,_=self.request(port,"/batch",{**identity,"batch_id":"later"})
     self.assertEqual(status,409)
    finally:
     process.terminate()
     try:process.wait(timeout=5)
     except subprocess.TimeoutExpired:process.kill();process.wait(timeout=3)
    self.assertEqual(process.returncode,0)

if __name__=="__main__":unittest.main()
