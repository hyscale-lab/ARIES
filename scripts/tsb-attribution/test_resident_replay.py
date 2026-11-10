import gzip,json,tempfile,unittest
from pathlib import Path
from replay import verify_resident_census
class ResidentCensusTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.p=Path(self.tmp.name)
  self.record={'resident_population':{'resident_gpa_pages':10,'unique_nonzero_host_pfns':8,'zero_pfn_pages':0},'sample_design':{'sample_pages':2,'population_pages':8,'requested_pages':2},'selection':[{'gpa':4096,'host_pfn':10,'host_kpageflags':0,'host_kpagecount':1},{'gpa':8192,'host_pfn':11,'host_kpageflags':2,'host_kpagecount':1}]}
  with gzip.open(self.p/'resident-sample.jsonl.gz','wt') as f:
   for g in (4096,8192):f.write(json.dumps({'gpa':g})+'\n')
  self.save()
 def save(self):(self.p/'resident-census.json').write_text(json.dumps(self.record))
 def tearDown(self):self.tmp.cleanup()
 def test_metadata_only_valid(self):
  summary,checks=verify_resident_census(self.p)
  self.assertFalse(summary['payload_read']);self.assertTrue(all(checks.values()));self.assertEqual(summary['sample_pages'],2)
 def test_duplicate_host_pfn(self):
  self.record['selection'][1]['host_pfn']=10;self.save()
  with self.assertRaisesRegex(ValueError,'duplicate'):verify_resident_census(self.p)
 def test_count_mismatch(self):
  self.record['sample_design']['sample_pages']=1;self.save()
  with self.assertRaisesRegex(ValueError,'count mismatch'):verify_resident_census(self.p)
 def test_population_mismatch(self):
  self.record['resident_population']['unique_nonzero_host_pfns']=7;self.save()
  with self.assertRaisesRegex(ValueError,'population'):verify_resident_census(self.p)
 def test_missing_rich_attribution(self):
  self.record['selection'][1]['gpa']=12288;self.save()
  with self.assertRaisesRegex(ValueError,'identity mismatch'):verify_resident_census(self.p)
 def test_zero_population_empty_selection(self):
  self.record={'resident_population':{'resident_gpa_pages':0,'unique_nonzero_host_pfns':0},'sample_design':{'sample_pages':0,'population_pages':0,'requested_pages':2},'selection':[]}
  self.save();(self.p/'resident-sample.jsonl.gz').unlink()
  self.assertEqual(verify_resident_census(self.p)[0]['sample_pages'],0)
if __name__=='__main__':unittest.main()
