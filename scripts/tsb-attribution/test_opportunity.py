import gzip,json,tempfile,unittest
from pathlib import Path
from analyze_opportunity import srs_total,compress_model,transitions,analyze

def compression():
 return {'page_bytes':4096,'codec':'zlib','level':1,'resident_population':{'resident_gpa_pages':4,'unique_nonzero_host_pfns':4,'pre_capture_vmm_rss_kib':20},
 'sample_design':{'population_pages':4,'sample_pages':2,'rule':'uniform without replacement','seed':1},
 'samples':[{'gpa':0,'category':'file_cache','raw_bytes':4096,'compressed_bytes':4096,'stored_bytes':4096,'saved_bytes':0,'compress_ns':1000000000,'decompress_ns':1000000000,'roundtrip_ok':True,'host_kpagecount':1},
 {'gpa':4096,'category':'anonymous','raw_bytes':4096,'compressed_bytes':2032,'stored_bytes':2048,'saved_bytes':2048,'compress_ns':1000000000,'decompress_ns':1000000000,'roundtrip_ok':True,'host_kpagecount':2}]}

class OpportunityTests(unittest.TestCase):
 def test_sample_expansion_and_shared_qualification(self):
  r=compress_model(compression(),10)
  self.assertEqual(r['estimated_saved_bytes']['estimate'],4096)
  self.assertEqual(r['instantaneous_static_gap_saved_byte_seconds'],40960)
  self.assertEqual(r['exclusive_mapping_sample_static_gap_saved_byte_seconds'],0)
  self.assertEqual(r['projected_codec_only_serial_compress_seconds'],4)
  self.assertEqual(r['codec_only_all_at_once_illustration_saved_byte_seconds'],8192)
  self.assertEqual(r['categories']['anonymous']['estimated_population_pages'],2)
 def test_census_uncertainty_zero_and_partial_uncertainty_nonzero(self):
  self.assertEqual(srs_total([0,4096],2)['standard_error'],0)
  self.assertGreater(srs_total([0,4096],4)['standard_error'],0)
 def test_invalid_roundtrip_fails(self):
  c=compression();c['samples'][0]['roundtrip_ok']=False
  with self.assertRaises(ValueError):compress_model(c,1)
 def test_hindsight_categories_use_correct_endpoint(self):
  p={0:('file_cache','observed'),4096:('anonymous','assumed')}
  c={4096:('reserved','observed'),8192:('shmem','observed')}
  r=transitions(p,c,2)
  self.assertEqual(r['previous_only_rectangle_byte_seconds_by_previous_category'],{'file_cache':8192})
  self.assertEqual(r['reused_pages_by_previous_category'],{'anonymous':1})
  self.assertEqual(r['newly_needed_pages_by_current_category'],{'shmem':1})
 def test_gap_excludes_all_observer_pauses_and_final_censor(self):
  with tempfile.TemporaryDirectory(dir='.cache') as td:
   base=Path(td);root=base/'observer/attribution';root.mkdir(parents=True)
   (root.parent/'selected-slots.json').write_text(json.dumps([{'guest_phys_addr':0,'memory_size':16384}]))
   b=[]
   for i,(arm,collect) in enumerate(((0,3000000000),(10000000000,13000000000))):
    for kind,t in [('arm',arm),('collect',collect)]:
     b.append({'kind':kind,'begin_ns':t,'pause_ack_ns':t+100,'resume_ack_ns':t+1000000000,'vm_id':'v','vmm_pid':123,'vmm_start_ticks':'1'})
    d=root/f'step-{i+1:06d}';d.mkdir()
    (d/'reading.json').write_text(json.dumps({'not_resident_at_end_gpa_ranges':[[8192,16384]]}))
    (d/'summary.json').write_text(json.dumps({'pages':1,'group_id':f'g{i}'}))
    with gzip.open(d/'pages.jsonl.gz','wt') as f:f.write(json.dumps({'gpa':4096*i,'category':'anonymous','access_status':'observed'})+'\n')
    (d/'compression.json').write_text(json.dumps(compression()))
   (root/'boundaries.jsonl').write_text(''.join(json.dumps(x)+'\n' for x in b))
   result=analyze(root,base/'out')
   self.assertEqual(result['totals']['sum_gap_seconds'],6)
   self.assertEqual(result['totals']['complete_gaps'],1)
   self.assertTrue(result['steps'][-1]['right_censored_after_last_tool'])
   self.assertEqual(result['totals']['unique_host_pfn_left_endpoint_rectangle_byte_seconds'],4*4096*6)
   self.assertEqual(result['totals']['resident_gpa_left_endpoint_rectangle_byte_seconds'],2*4096*6)
   # The current study collects metadata only: no compression file or payload.
   for d in sorted(root.glob('step-*')):
    c=json.loads((d/'compression.json').read_text());(d/'compression.json').unlink()
    census={k:c[k] for k in ('resident_population','sample_design')}
    census['resident_population'].pop('pre_capture_vmm_rss_kib')
    census['resident_population']['pre_capture_vmm_smaps_rollup']={'Rss_kib':20}
    (d/'resident-census.json').write_text(json.dumps(census))
    with gzip.open(d/'resident-sample.jsonl.gz','wt') as f:
     for x in c['samples']:f.write(json.dumps({'gpa':x['gpa'],'category':x['category'],'process_names':[]})+'\n')
   meta=analyze(root,base/'metadata-only')
   self.assertFalse(meta['totals']['compression_measured'])
   self.assertIsNone(meta['totals']['instantaneous_static_gap_saved_byte_seconds'])
   self.assertEqual(meta['totals']['resident_category_estimated_rectangle_byte_seconds'],{'anonymous':49152,'file_cache':49152})
   self.assertEqual(meta['totals']['vmm_rss_left_endpoint_rectangle_byte_seconds'],20*1024*6)

if __name__=='__main__':unittest.main()
