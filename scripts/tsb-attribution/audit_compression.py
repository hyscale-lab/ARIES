#!/usr/bin/env python3
"""Validate saved compression frames and sample bookkeeping without live memory."""
import argparse,gzip,hashlib,json,zlib
from pathlib import Path

def audit(step):
 step=Path(step);p=step/'compression.json';x=json.loads(p.read_text());samples=x['samples'];design=x['sample_design'];pop=x['resident_population']
 checks={}
 checks['sample_count_matches_design']=len(samples)==design['sample_pages']<=design['population_pages']
 checks['population_matches_census']=design['population_pages']==pop['unique_nonzero_host_pfns']<=pop['resident_gpa_pages']
 checks['unique_sample_gpas']=len({s['gpa'] for s in samples})==len(samples)
 checks['unique_sample_host_pfns']=len({s['host_pfn'] for s in samples})==len(samples) and all(s['host_pfn']>0 for s in samples)
 payload=(step/'compressed-sample-data.bin').read_bytes();cursor=0;failures=[]
 overhead=x['metadata_bytes_per_compressed_page_assumption']
 for i,s in enumerate(samples):
  if s['payload_offset']!=cursor:failures.append({'sample':i,'reason':'noncontiguous frame offset'})
  packed=payload[s['payload_offset']:s['payload_offset']+s['payload_length']];cursor=s['payload_offset']+s['payload_length']
  try:
   raw=zlib.decompress(packed)
   valid=(len(packed)==s['compressed_bytes']==s['payload_length'] and len(raw)==x['page_bytes']==s['raw_bytes'] and hashlib.sha256(raw).hexdigest()==s['raw_sha256'] and s['stored_bytes']==min(s['raw_bytes'],len(packed)+overhead) and s['saved_bytes']==s['raw_bytes']-s['stored_bytes'] and s['roundtrip_ok'])
   if not valid:failures.append({'sample':i,'reason':'frame/hash/storage inconsistency'})
  except zlib.error as e:failures.append({'sample':i,'reason':str(e)})
 checks['all_frames_independently_restore_and_match_hash']=not failures
 checks['payload_length_matches_last_frame']=cursor==len(payload)
 rp=step/'resident-sample.jsonl.gz'
 if rp.exists():
  rows=[json.loads(l) for l in gzip.open(rp,'rt') if l.strip()]
  by={r['gpa']:r for r in rows}
  checks['resident_sample_row_census']=len(rows)==len(by)==len(samples)
  checks['sample_categories_match_attribution']=all(s['gpa'] in by and s['category']==by[s['gpa']]['category'] for s in samples)
 return {'step':str(step),'passed':all(checks.values()),'checks':checks,'sample_count':len(samples),'failures':failures,
 'compression_json_sha256':hashlib.sha256(p.read_bytes()).hexdigest(),'payload_sha256':hashlib.sha256(payload).hexdigest(),
 'limits':['Validates retained frames and bookkeeping, not re-observation of destroyed VM or full population census','Uniform selection is supported by recorded source/design, population list is not retained for independent reselection']}

if __name__=='__main__':
 p=argparse.ArgumentParser();p.add_argument('--attribution-root',required=True);p.add_argument('--output',required=True);a=p.parse_args()
 root=Path(a.attribution_root);results=[audit(s.parent) for s in sorted(root.glob('step-*/compression.json'))]
 out={'passed':bool(results) and all(r['passed'] for r in results),'steps':results,'sample_count':sum(r['sample_count'] for r in results)}
 Path(a.output).parent.mkdir(parents=True,exist_ok=True);Path(a.output).write_text(json.dumps(out,indent=2)+'\n')
 print(json.dumps({'passed':out['passed'],'steps':len(results),'samples':out['sample_count']}))
 if not out['passed']:raise SystemExit(1)
