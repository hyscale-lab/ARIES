import json
from pathlib import Path
import tempfile
import unittest
from capture_memory import CaptureFailure
from process_adapter import ExternalCapture

class API:
 def __init__(self):self.calls=[];self.child=None
 def call(self,endpoint,operation,**fields):
  assert self.child.process.poll() is not None,"abort RPC preceded child reap"
  self.calls.append((endpoint,operation,fields))
  if operation=="info":
   return {"schema":"aries.firecracker.metadata-capture.v1","capture_id":fields["capture_id"],
           "vm_generation":fields["vm_generation"],"epoch":self.child.epoch,"state":"open"}
  return {"capture_record_id":"aborted"}

class ProcessTests(unittest.TestCase):
 def test_explicit_failed_rotation_cleanup_discards_pending_seal(self):
  with tempfile.TemporaryDirectory() as directory:
   root=Path(directory);(root/"capture.py").write_text("import time\ntime.sleep(30)\n")
   class CleanupAPI(API):
    def call(self,endpoint,operation,**fields):
     super().call(endpoint,operation,**fields)
     return {"schema":"aries.firecracker.metadata-capture.v1","capture_id":"c","vm_generation":"generation",
             "epoch":1,"consistency":"discarded","state":"aborted"}
   api=CleanupAPI();child=ExternalCapture(root,api,"/unused","c","generation",1,root/"out");api.child=child
   child.start(config=root/"unused");child.sealed=True
   result=child.discard_for_cleanup("collector poisoned")
   self.assertFalse(result["consistency_valid"])
   self.assertTrue(result["original_sealed_evidence_preserved"])
   self.assertEqual(api.calls[0][1],"abort")
   self.assertIn("failed rotation",api.calls[0][2]["reason"])

 def test_parent_cancel_after_seal_retains_records_without_abort_rpc(self):
  with tempfile.TemporaryDirectory() as directory:
   root=Path(directory);(root/"capture.py").write_text("import time\ntime.sleep(30)\n")
   api=API();child=ExternalCapture(root,api,"/unused","c","generation",1,root/"out");api.child=child
   child.start(config=root/"unused");child.sealed=True
   result=child.abort("publication timeout")
   self.assertTrue(result["sealed_records_retained"])
   self.assertEqual(api.calls,[])

 def test_post_seal_publication_failure_does_not_abort_closed_capture(self):
  from unittest.mock import patch,Mock
  import capture
  with tempfile.TemporaryDirectory() as directory:
   root=Path(directory);config=root/"config.json";config.write_text("{}")
   provider=Mock(state="open",capture_record_id=None,regions=[{"guest_base":0,"bytes":4096}],
                 vm_generation="g",epoch=None,begin={"capture_id":"c","vm_generation":"g"})
   def seal():
    provider.state="sealed";provider.capture_record_id="sealed-1"
    return {"capture_record_id":"sealed-1","consistency":"quiescence_only"}
   provider.seal.side_effect=seal
   decoder=Mock();decoder.bootstrap.return_value={"anchors":{"kernel_sha256":"hash"},"layout":{"kernel_sha256":"hash"}}
   argv=["capture.py","bootstrap","--socket","/unused","--capture-id","c","--output",str(root/"out"),
         "--config",str(config),"--consistency","quiescence_only"]
   with patch("sys.argv",argv),patch.object(capture,"VmmAPI",return_value=Mock(calls=[])),patch.object(capture,"CaptureProvider",return_value=provider),patch.object(capture,"GuestDecoder",return_value=decoder),patch.object(capture,"publish_vmm_capture",side_effect=OSError("publication failed")):
    with self.assertRaises(OSError):capture.main()
   provider.abort.assert_not_called()
   failure=json.loads((root/"out/failure.json").read_text())
   self.assertTrue(failure["live_reads_closed"])
   self.assertTrue(failure["publication_failed"])
   self.assertTrue(failure["attribution_decode_completed"])
   self.assertFalse(failure["records_acknowledged"])

 def test_cli_rejects_missing_inputs_before_api(self):
  from unittest.mock import patch
  import capture
  with tempfile.TemporaryDirectory() as directory:
   with patch("sys.argv",["capture.py","bootstrap","--socket","/unused","--capture-id","c","--output",str(Path(directory)/"out")]), patch.object(capture,"VmmAPI") as api:
    with self.assertRaises(SystemExit):capture.main()
    api.assert_not_called()
   config=Path(directory)/"config.json";config.write_text(json.dumps({"enable_compression":True}))
   with patch("sys.argv",["capture.py","bootstrap","--socket","/unused","--capture-id","c","--output",str(Path(directory)/"out"),"--config",str(config)]), patch.object(capture,"VmmAPI") as api:
    with self.assertRaises(SystemExit):capture.main()
    api.assert_not_called()

 def test_timeout_reaps_child_before_capture_abort(self):
  with tempfile.TemporaryDirectory() as directory:
   root=Path(directory);(root/"capture.py").write_text("import time\ntime.sleep(30)\n")
   api=API();child=ExternalCapture(root,api,"/not-used","c","generation",1,root/"out",timeout_seconds=.02);api.child=child
   child.start(config=root/"unused.json")
   with self.assertRaises(CaptureFailure):child.await_sealed()
   self.assertIsNotNone(child.process.poll())
   self.assertEqual([(e,o) for e,o,_ in api.calls],[("/metadata-capture","info"),("/metadata-capture","abort")])
 def test_mismatched_seal_file_is_not_read_closure(self):
  with tempfile.TemporaryDirectory() as directory:
   root=Path(directory);(root/"capture.py").write_text("""
import json,sys,time
from pathlib import Path
out=Path(sys.argv[sys.argv.index('--output')+1]);out.mkdir()
(out/'capture-begin.json').write_text(json.dumps({'capture_id':'c','vm_generation':'generation','epoch':1}))
(out/'capture-seal.json').write_text(json.dumps({'schema':'aries.firecracker.metadata-capture.v1','capture_id':'other','vm_generation':'generation','epoch':1,'state':'sealed','consistency':'pending','capture_record_id':'sealed'}))
time.sleep(30)
""")
   api=API();child=ExternalCapture(root,api,"/unused","c","generation",1,root/"out");api.child=child
   child.start(config=root/"unused")
   with self.assertRaises(CaptureFailure):child.await_sealed()
   self.assertFalse(child.sealed)
   self.assertIsNotNone(child.process.poll())
   self.assertEqual([operation for _,operation,_ in api.calls],["info","abort"])

 def test_seal_barrier_does_not_wait_for_publication(self):
  with tempfile.TemporaryDirectory() as directory:
   root=Path(directory);(root/"capture.py").write_text("""
import json,sys,time
from pathlib import Path
out=Path(sys.argv[sys.argv.index('--output')+1]);out.mkdir()
(out/'capture-begin.json').write_text(json.dumps({'capture_id':'c','vm_generation':'generation','epoch':1}))
(out/'capture-seal.json').write_text(json.dumps({'schema':'aries.firecracker.metadata-capture.v1','capture_id':'c','vm_generation':'generation','epoch':1,'state':'sealed','consistency':'pending','capture_record_id':'sealed'}))
time.sleep(.3)
""")
   api=API();child=ExternalCapture(root,api,"/unused","c","generation",1,root/"out");api.child=child
   child.start(config=root/"unused")
   self.assertEqual(child.await_sealed()["capture_record_id"],"sealed")
   self.assertIsNone(child.process.poll())
   self.assertEqual(child.await_publication()["returncode"],0)
   self.assertEqual(api.calls,[])

if __name__=="__main__":unittest.main()
