import json
from pathlib import Path
import tempfile
import unittest
from capture_memory import CaptureFailure
from epochs import EpochCoordinator

class API:
 def __init__(self):self.calls=[];self.stale=False
 def call(self,endpoint,operation,**fields):
  self.calls.append((endpoint,operation,fields))
  common={"schema":"aries.firecracker.trace-epochs.v1","vm_generation":"wrong" if self.stale else "vm-1"}
  if operation=="arm":
   return {**common,"active_epoch":1,"epoch":1,"regions":[{"slot":0,"guest_base":0,"bytes":4096,"page_count":1}],
           "baseline_discarded":True,"selected_metrics":fields["metrics"],"epoch_record_id":"record-1","census_record_id":"record-1","access_record_id":None,"dirty_record_id":None}
  if operation=="rotate":
   return {**common,"epoch":fields["epoch"],"next_epoch":fields["next_epoch"],"active_epoch":fields["next_epoch"],"regions":[{"slot":0,"guest_base":0,"bytes":4096,"page_count":1}]}
  if operation=="export":
   return {**common,"record_id":fields["record_id"],"kind":fields["kind"],"offset":0,"next_offset":1,"total_rows":1,
           "complete":True,"metadata":{"epoch":1},"rows":[{"gpa":0,"pfn":1,"host_kpagecount":1,"host_kpageflags":4}]}
  if operation=="ack":return common
  raise AssertionError(operation)

class EpochTests(unittest.TestCase):
 def test_selected_metrics_export_before_generation_guarded_ack(self):
  api=API();c=EpochCoordinator(api,["census"]);r=c.arm("llm_wait")
  with tempfile.TemporaryDirectory() as directory:
   manifest=c.publish(r,Path(directory)/"record")
   self.assertTrue(manifest["complete"])
   self.assertTrue((Path(directory)/"record/manifest.json").exists())
  self.assertEqual(api.calls[0][2]["metrics"],["census"])
  self.assertEqual(api.calls[-1][1],"ack")
  self.assertEqual(api.calls[-1][2]["vm_generation"],"vm-1")
 def test_bad_rotate_identity_poisoned_no_second_reset(self):
  api=API();c=EpochCoordinator(api,["dirty"]);c.arm("tool");api.stale=True
  with self.assertRaises(CaptureFailure):c.rotate("llm_wait")
  count=len(api.calls)
  with self.assertRaises(CaptureFailure):c.rotate("llm_wait")
  self.assertEqual(len(api.calls),count)

 def test_changed_regions_poison_before_any_second_rotation(self):
  api=API();c=EpochCoordinator(api,["census"]);c.arm("tool")
  original=api.call
  def changed(*args,**kwargs):
   result=original(*args,**kwargs)
   if args[1]=="rotate":result["regions"][0]["bytes"]=8192
   return result
  api.call=changed
  with self.assertRaises(CaptureFailure):c.rotate("llm_wait")
  self.assertTrue(c.failed)

if __name__=="__main__":unittest.main()
