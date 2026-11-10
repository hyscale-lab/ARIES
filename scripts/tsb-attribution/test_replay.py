import gzip,hashlib,json,tempfile,unittest,zlib
from pathlib import Path
from replay import verify_compression
class CompressionEvidenceTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.p=Path(self.tmp.name)
  self.raw=b'A'*4096;self.packed=zlib.compress(self.raw,1)
  self.sample={'gpa':4096,'payload_offset':0,'payload_length':len(self.packed),'compressed_bytes':len(self.packed),'raw_bytes':4096,'raw_sha256':hashlib.sha256(self.raw).hexdigest(),'roundtrip_ok':True,'category':'anonymous','process_names':['fixture'],'in_step_footprint':True,'stored_bytes':min(4096,len(self.packed)+16),'saved_bytes':max(0,4096-len(self.packed)-16)}
  self.record={'codec':'zlib','level':1,'page_bytes':4096,'metadata_bytes_per_compressed_page_assumption':16,'sample_design':{'sample_pages':1},'samples':[self.sample]}
  (self.p/'compressed-sample-data.bin').write_bytes(self.packed)
  with gzip.open(self.p/'resident-sample.jsonl.gz','wt') as f:f.write(json.dumps({'gpa':4096,'category':'anonymous','process_names':['fixture'],'access_status':'observed'})+'\n')
  self.save()
 def save(self):(self.p/'compression.json').write_text(json.dumps(self.record))
 def tearDown(self):self.tmp.cleanup()
 def test_valid_payload_and_arithmetic(self):
  summary,checks=verify_compression(self.p);self.assertEqual(summary['samples'],1);self.assertTrue(all(checks.values()))
 def test_corrupted_raw_hash(self):
  self.sample['raw_sha256']='0'*64;self.save()
  with self.assertRaisesRegex(ValueError,'hash mismatch'):verify_compression(self.p)
 def test_trailing_unindexed_payload(self):
  with (self.p/'compressed-sample-data.bin').open('ab') as f:f.write(b'extra')
  with self.assertRaisesRegex(ValueError,'trailing'):verify_compression(self.p)
 def test_noncontiguous_offset(self):
  self.sample['payload_offset']=1;self.save()
  with self.assertRaisesRegex(ValueError,'noncontiguous'):verify_compression(self.p)
 def test_category_not_same_as_attribution(self):
  self.sample['category']='file_cache';self.save()
  with self.assertRaisesRegex(ValueError,'category/process'):verify_compression(self.p)
 def test_forged_savings(self):
  self.sample['saved_bytes']+=1;self.save()
  with self.assertRaisesRegex(ValueError,'arithmetic'):verify_compression(self.p)
if __name__=='__main__':unittest.main()
