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
import threading,time
class VmmObserver:
 def __init__(self,*args):
  self.start=1;self.abort=threading.Event();self.lock=threading.Lock()
  self.interrupts=0;self.releases=0;self.cleanup_started=threading.Event()
 def session_start(self):
  self.abort.wait(5)
  if self.abort.is_set():raise RuntimeError("initialization canceled")
  return {"ok":True}
 def request_abort(self):
  with self.lock:
   if self.abort.is_set():return
   self.abort.set();self.interrupts+=1
 def abort_session(self,reason):
  self.cleanup_started.set();time.sleep(.08)
  self.releases+=1
  return {"ok":True,"released":True,"paused":False,"measurement_valid":False,
          "terminal_complete":False,"interrupts":self.interrupts,"release_attempt":self.releases}
 def close(self):pass
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
