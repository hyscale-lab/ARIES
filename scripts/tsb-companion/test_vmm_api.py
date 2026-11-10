import base64
import hashlib
import unittest
from capture_memory import CaptureFailure
from vmm_api import CaptureProvider, export_rows, finalize_capture, VmmRequestError

class API:
 def __init__(self):
  self.calls=[];self.corrupt=False;self.capture_id=None;self.epoch=None
 def call(self,endpoint,operation,**kw):
  if operation=="begin":self.capture_id=kw["capture_id"];self.epoch=kw.get("epoch")
  result=self._respond(endpoint,operation,**kw)
  result.setdefault("schema","aries.firecracker."+endpoint.strip("/")+".v1")
  result.setdefault("vm_generation","vm-generation")
  result.setdefault("capture_id",self.capture_id);result.setdefault("epoch",self.epoch)
  return result
 def _respond(self,endpoint,operation,**kw):
  self.calls.append((endpoint,operation,kw))
  if operation=="begin":
   return {"schema":"aries.firecracker.metadata-capture.v1","capture_id":kw["capture_id"],"state":"open",
    "vm_generation":"vm-generation","epoch":kw.get("epoch"),"census_record_id":"census-1",
    "regions":[{"slot":0,"guest_base":0,"bytes":16384,"page_count":4}],
    "budgets":{"max_bytes":kw["max_bytes"],"max_scan_bytes":kw["max_scan_bytes"]}}
  if operation=="read_pages":
   data=bytes(4096);sha=hashlib.sha256(data).hexdigest()
   return {"pages":[{"gpa":g,"status":"ok","data_b64":base64.b64encode(data).decode(),
                    "sha256":"bad" if self.corrupt else sha,"cache_hit":False} for g in kw["gpas"]]}
  if operation=="seal":return {"capture_record_id":"sealed-1","consistency":"quiescence_only","state":"sealed"}
  if operation=="abort":return {"capture_record_id":"aborted-1"}
  if operation=="export":
   rows=[{"gpa":0,"pfn":12,"host_kpageflags":4,"host_kpagecount":1},
         {"gpa":4096,"pfn":12,"host_kpageflags":4,"host_kpagecount":1},
         {"gpa":8192,"pfn":13,"host_kpageflags":5,"host_kpagecount":1},
         {"gpa":12288,"pfn":0,"host_kpageflags":None,"host_kpagecount":None}]
   lo=kw["offset"];hi=min(len(rows),lo+kw["limit"])
   return {"record_id":kw["record_id"],"kind":kw["kind"],"offset":lo,"next_offset":hi,
           "total_rows":len(rows),"complete":hi==len(rows),"rows":rows[lo:hi],
           "metadata":{"resident_population":{"resident_gpa_pages":4,"unique_nonzero_host_pfns":2,"zero_pfn_pages":1,
                        "vmm_rss_kib":32,"vmm_pss_kib":24},"inventory_begin_ns":100,"inventory_end_ns":200}}
  raise AssertionError(operation)

class ProviderTests(unittest.TestCase):
 def test_metadata_only_never_requires_idle_or_dirty_endpoints(self):
  api=API();p=CaptureProvider(api,"capture-1",consistency="quiescence_only")
  self.assertEqual(p.budgets["max_scan_bytes"],0)
  self.assertEqual(p.read_pages([0])[0],bytes(4096))
  p.seal()
  with self.assertRaises(CaptureFailure):p.read_pages([4096])
  self.assertEqual({x[0] for x in api.calls},{"/metadata-capture"})
 def test_census_alias_dedup_sample_and_zero_mask(self):
  api=API();p=CaptureProvider(api,"capture-1",consistency="quiescence_only")
  sample=p.resident_sample(42,4)
  self.assertEqual([r["host_pfn"] for r in sample["selection"]],[12,13])
  self.assertEqual([r["gpa"] for r in sample["selection"]],[0,8192])
  self.assertEqual(sample["resident_population"]["zero_pfn_pages"],1)
 def test_corrupt_bytes_and_oversized_request_fail(self):
  api=API();p=CaptureProvider(api,"capture-1",consistency="quiescence_only")
  api.corrupt=True
  with self.assertRaises(CaptureFailure):p.read_pages([0])
  self.assertEqual(len(p.read_receipts),1)
  with self.assertRaises(CaptureFailure):p.read_pages(list(range(257)))
 def test_export_bounded_pagination(self):
  rows=[r for batch,_ in export_rows(API(),"/memory-census","census-1","resident",limit=2,max_rows=4,expected_generation="vm-generation") for r in batch]
  self.assertEqual(len(rows),4)
  with self.assertRaises(CaptureFailure):list(export_rows(API(),"/memory-census","census-1","resident",max_rows=3,expected_generation="vm-generation"))

 def test_restart_reused_record_and_changed_total_are_rejected(self):
  api=API()
  with self.assertRaises(CaptureFailure):
   list(export_rows(api,"/memory-census","census-1","resident",expected_generation="other-vm"))
  original=api.call
  def mutate(endpoint,operation,**kw):
   result=original(endpoint,operation,**kw)
   if kw.get("offset",0):
    result["total_rows"]=5;result["complete"]=False
   return result
  api.call=mutate
  with self.assertRaises(CaptureFailure):
   list(export_rows(api,"/memory-census","census-1","resident",limit=2,expected_generation="vm-generation"))
 def test_open_or_wrong_consistency_seal_response_is_not_read_closure(self):
  for field,value in (("state","open"),("capture_record_id",""),("consistency","valid")):
   api=API();p=CaptureProvider(api,"capture-1",consistency="quiescence_only");original=api.call
   def malformed(endpoint,operation,**kw):
    result=original(endpoint,operation,**kw);result[field]=value;return result
   api.call=malformed
   with self.assertRaises(CaptureFailure):p.seal()
   self.assertEqual(p.state,"open")

 def test_only_explicit_matching_nonmutating_backpressure_is_retryable(self):
  body={"schema":"aries.firecracker.metadata-capture.v1","vm_generation":"vm-generation",
        "error":{"code":"backpressure","mutation_performed":False,"reset_performed":False,"poisoned":False}}
  self.assertTrue(VmmRequestError("/metadata-capture","begin",400,body).is_safe_backpressure("vm-generation"))
  self.assertFalse(VmmRequestError("/metadata-capture","begin",400,body).is_safe_backpressure("new-generation"))
  for field in ("mutation_performed","reset_performed","poisoned"):
   changed={**body,"error":{**body["error"],field:True}}
   self.assertFalse(VmmRequestError("/metadata-capture","begin",400,changed).is_safe_backpressure("vm-generation"))
  for malformed in ({},[],{**body,"error":"backpressure"},{**body,"error":{"code":"backpressure"}}):
   self.assertFalse(VmmRequestError("/metadata-capture","begin",400,malformed).is_safe_backpressure("vm-generation"))

 def test_old_sealed_info_while_new_capture_is_open(self):
  class MultiAPI:
   def __init__(self):self.captures={}
   def call(self,endpoint,operation,**kw):
    cid=kw["capture_id"]
    if operation=="begin":
     self.captures[cid]={"epoch":kw.get("epoch"),"state":"open"}
    state=self.captures[cid]
    result={"schema":"aries.firecracker.metadata-capture.v1","vm_generation":"vm-generation",
            "capture_id":cid,"epoch":state["epoch"],"state":state["state"]}
    if operation=="begin":
     return {**result,"census_record_id":"census-"+cid,"regions":[{"guest_base":0,"bytes":4096,"page_count":1}],
             "budgets":{"max_bytes":kw["max_bytes"],"max_scan_bytes":kw["max_scan_bytes"]}}
    if operation=="seal":
     state["state"]="sealed"
     return {**result,"state":state["state"],"capture_record_id":"record-"+cid,"consistency":"quiescence_only"}
    if operation=="info":
     return {**result,"state":state["state"],"capture_record_id":"record-"+cid,"consistency":"quiescence_only"}
    raise AssertionError(operation)
  api=MultiAPI()
  old=CaptureProvider(api,"old",consistency="quiescence_only",epoch=1);old.seal()
  new=CaptureProvider(api,"new",consistency="quiescence_only",epoch=2)
  self.assertEqual(old.info()["capture_id"],"old")
  self.assertEqual(old.info()["epoch"],1)
  self.assertEqual(new.info()["state"],"open")

 def test_abort_includes_explicit_reason(self):
  api=API();p=CaptureProvider(api,"capture-1",epoch=7,vm_generation="vm-generation")
  p.abort("test cancellation")
  self.assertEqual(api.calls[-1][2]["reason"],"test cancellation")

 def test_capture_id_epoch_and_final_verdict_are_checked(self):
  api=API();p=CaptureProvider(api,"capture-1",epoch=7,vm_generation="vm-generation")
  original=api.call
  def stale(endpoint,operation,**kw):
   result=original(endpoint,operation,**kw);result["epoch"]=8;return result
  api.call=stale
  with self.assertRaises(CaptureFailure):p.read_pages([0])
  class FinalAPI:
   def call(self,*args,**kwargs):
    return {"schema":"aries.firecracker.metadata-capture.v1","capture_id":"capture-1",
            "vm_generation":"vm-generation","epoch":7,"capture_record_id":"final-2",
            "sealed_record_id":"sealed-1","consistency":"pending"}
  begin={"capture_id":"capture-1","vm_generation":"vm-generation","epoch":7}
  with self.assertRaises(CaptureFailure):finalize_capture(FinalAPI(),begin,{"capture_record_id":"sealed-1"})

if __name__=="__main__":unittest.main()
