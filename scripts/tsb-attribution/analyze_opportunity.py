#!/usr/bin/env python3
"""Retrospective memory-times-gap opportunity models; not measured reclamation."""
import argparse,collections,gzip,hashlib,json,math,statistics
from pathlib import Path

PAGE=4096

def digest(path):
 h=hashlib.sha256()
 with Path(path).open('rb') as f:
  for b in iter(lambda:f.read(1048576),b''):h.update(b)
 return h.hexdigest()

def srs_total(values,population):
 n=len(values)
 if not n or n>population:raise ValueError('invalid SRS population/sample')
 mean=statistics.mean(values);estimate=population*mean
 if n==population:se=0.0
 elif n>=2:se=population*math.sqrt((1-n/population)*statistics.variance(values)/n)
 else:se=None
 return {'estimate':estimate,'sample_n':n,'population_n':population,'standard_error':se,
         'normal_approx_95_interval':None if se is None else [max(0,estimate-1.96*se),estimate+1.96*se],
         'interval_scope':'random-sampling design uncertainty only; not measurement, shared-page, persistence or implementation uncertainty'}

def compress_model(record,gap):
 design=record['sample_design'];samples=record['samples'];n=design['sample_pages'];N=design['population_pages']
 if len(samples)!=n:raise ValueError('compression sample count mismatch')
 if record['page_bytes']!=PAGE:raise ValueError('unsupported page size')
 if len({s['gpa'] for s in samples})!=n:raise ValueError('duplicate sampled representative GPA')
 for s in samples:
  if not s['roundtrip_ok'] or s['raw_bytes']!=PAGE or s['stored_bytes']>PAGE or s['stored_bytes']<0 or s['saved_bytes']!=PAGE-s['stored_bytes']:
   raise ValueError('invalid compression result')
 saved=srs_total([s['saved_bytes'] for s in samples],N)
 comp_ns=srs_total([s['compress_ns'] for s in samples],N)['estimate']
 decomp_ns=srs_total([s['decompress_ns'] for s in samples],N)['estimate']
 categories={}
 for c in sorted({s['category'] for s in samples}):
  selected=[s for s in samples if s['category']==c]
  cat_saved=srs_total([s['saved_bytes'] if s['category']==c else 0 for s in samples],N)
  categories[c]={'sample_pages':len(selected),'estimated_population_pages':N*len(selected)/n,
    'sample_saved_fraction':sum(s['saved_bytes'] for s in selected)/(PAGE*len(selected)),
    'estimated_saved_bytes':cat_saved['estimate'],'estimated_saved_byte_seconds':cat_saved['estimate']*gap,
    'sample_host_exclusive_pages':sum(s.get('host_kpagecount')==1 for s in selected),
    'sample_host_nonexclusive_pages':sum(s.get('host_kpagecount') is not None and s.get('host_kpagecount')!=1 for s in selected),
    'sample_host_mapcount_unknown_pages':sum(s.get('host_kpagecount') is None for s in selected)}
 exclusive=srs_total([s['saved_bytes'] if s.get('host_kpagecount')==1 else 0 for s in samples],N)
 codec_seconds=(comp_ns+decomp_ns)/1e9
 return {'codec':record.get('codec'),'level':record.get('level'),'sample_design':design,
  'estimated_saved_bytes':saved,'sample_saved_fraction':sum(s['saved_bytes'] for s in samples)/(n*PAGE),
  'instantaneous_static_gap_saved_byte_seconds':saved['estimate']*gap,
  'exclusive_mapping_sample_estimated_saved_bytes':exclusive,
  'exclusive_mapping_sample_static_gap_saved_byte_seconds':exclusive['estimate']*gap,
  'projected_codec_only_serial_compress_seconds':comp_ns/1e9,
  'projected_codec_only_serial_decompress_seconds':decomp_ns/1e9,
  'codec_only_all_at_once_illustration_saved_byte_seconds':saved['estimate']*max(gap-codec_seconds,0),
  'categories':categories,
  'limitations':['No pages evicted; no measured physical memory savings',
  'All gap estimates assume captured contents/category/population persist throughout the gap',
  'PFNs shared outside VM may not release physical host memory; kpagecount==1 does not prove unpinned/reclaimable',
  'Codec-only illustration excludes memory reads/copy/allocation/bookkeeping/pfault/device latency and is not a conservative guarantee',
  'No observed samples in a category does not prove category absence',
  'Per-page storage model supplied by collector, not implemented compressed-memory representation']}

def resident_model(record,samples,gap):
 design=record['sample_design'];n=design['sample_pages'];N=design['population_pages']
 if len(samples)!=n or len({r['gpa'] for r in samples})!=n or not 0<n<=N:
  raise ValueError('invalid resident attribution sample')
 if N!=record['resident_population']['unique_nonzero_host_pfns']:raise ValueError('resident population mismatch')
 categories={};processes=collections.Counter()
 for r in samples:
  for name in set(r.get('process_names',[])):processes[name]+=1
 for c in sorted({r['category'] for r in samples}):
  selected=[r for r in samples if r['category']==c]
  estimate=srs_total([PAGE if r['category']==c else 0 for r in samples],N)
  categories[c]={'sample_pages':len(selected),'estimated_population_pages':N*len(selected)/n,
   'estimated_resident_bytes':estimate,'left_endpoint_rectangle_byte_seconds':estimate['estimate']*gap,
   'sample_pages_with_process_name':sum(bool(r.get('process_names')) for r in selected),
   'ownership_status_counts':dict(collections.Counter(r.get('ownership_status','not_recorded') for r in selected)),
   'reserved_region_counts':dict(collections.Counter(r.get('reserved_region') or 'not_applicable' for r in selected)) if c=='reserved' else {}}
 return {'sample_design':design,'categories':categories,
  'process_name_estimated_population_pages_overlapping':{k:N*v/n for k,v in processes.items()},
  'process_name_sample_pages_overlapping':dict(processes),
  'scope':'uniform sample of distinct resident host-PFN representatives; category composition estimate, not touched footprint or measured resident integral',
  'limits':['No payload compression measured by this model','No observed sample for a category does not prove absence','Process-name totals overlap for shared pages','Static gap categories/population may change while guest runs']}

def read_pages(path):
 result={}
 with gzip.open(path,'rt') as f:
  for line in f:
   r=json.loads(line);g=r['gpa']
   if g in result:raise ValueError('duplicate GPA in attribution')
   result[g]=(r['category'],r.get('access_status','unknown'))
 return result

def transitions(previous,current,gap):
 cats=collections.Counter(c for c,a in previous.values())
 old=collections.Counter();reused=collections.Counter();new=collections.Counter()
 for g,(c,a) in previous.items():
  (reused if g in current else old)[c]+=1
 for g,(c,a) in current.items():
  if g not in previous:new[c]+=1
 return {'prior_counted_pages_by_category':dict(cats),
  'prior_counted_rectangle_byte_seconds_by_category':{c:n*PAGE*gap for c,n in cats.items()},
  'previous_only_pages_by_previous_category':dict(old),
  'previous_only_rectangle_byte_seconds_by_previous_category':{c:n*PAGE*gap for c,n in old.items()},
  'reused_pages_by_previous_category':dict(reused),'newly_needed_pages_by_current_category':dict(new),
  'scope':'Observed-plus-assumed accessed sets; hindsight next-tool nonreuse, not safe discard, actual traffic, or whole-RAM categorization'}

def resident_gpa_count(reading,slots):
 # Final absent ranges plus slot extents form a census, including shared backing GPAs.
 total=sum(s['memory_size']//PAGE for s in slots)
 absent=reading.get('not_resident_at_end_gpa_ranges')
 if absent is None:return None
 n=sum((hi-lo)//PAGE for lo,hi in absent)
 return total-n

def analyze(root,output,allow_missing=False):
 root=Path(root);output=Path(output);output.mkdir(parents=True,exist_ok=True)
 paths=sorted(root.glob('step-*'));bounds=[json.loads(l) for l in (root/'boundaries.jsonl').read_text().splitlines() if l.strip()]
 relevant=[b for b in bounds if b['kind'] in ('arm','collect')]
 if len(relevant)!=2*len(paths):raise ValueError('boundary/capture census mismatch')
 arms=relevant[::2];collects=relevant[1::2]
 if any(a['kind']!='arm' or c['kind']!='collect' for a,c in zip(arms,collects)):raise ValueError('invalid boundary order')
 for b in relevant:
  if 'error' in b or 'resume_error' in b or b['resume_ack_ns']<b['pause_ack_ns']:raise ValueError('failed/inverted pause boundary')
 identity={(b['vm_id'],b['vmm_pid'],b['vmm_start_ticks']) for b in relevant}
 if len(identity)!=1:raise ValueError('cross-VM attribution series')
 slots_path=root.parent/'selected-slots.json';slots=json.loads(slots_path.read_text()) if slots_path.exists() else []
 inputs={str(root/'boundaries.jsonl'):digest(root/'boundaries.jsonl')};perstep=[];gaps=[];previous=None
 group_path=root.parent/'groups.jsonl';group_arms={}
 if group_path.exists():
  inputs[str(group_path)]=digest(group_path)
  for line in group_path.read_text().splitlines():
   if line.strip():
    g=json.loads(line)
    if g.get('event')=='batch_begin':group_arms[g['group_id']]=g.get('observer_arm',{})
 for i,path in enumerate(paths):
  for name in ('reading.json','pages.jsonl.gz','summary.json'):
   inputs[str(path/name)]=digest(path/name)
  reading=json.loads((path/'reading.json').read_text());summary=json.loads((path/'summary.json').read_text());pages=read_pages(path/'pages.jsonl.gz')
  if len(pages)!=summary['pages']:raise ValueError('page row summary count mismatch')
  row={'step':path.name,'group_id':summary['group_id'],'page_occurrences':len(pages),'categories':dict(collections.Counter(c for c,a in pages.values())),
   'observed_pages':sum(a=='observed' for c,a in pages.values()),'assumed_pages':sum(a=='assumed' for c,a in pages.values()),
   'arm_pause_seconds':(arms[i]['resume_ack_ns']-arms[i]['pause_ack_ns'])/1e9,
   'collect_pause_seconds':(collects[i]['resume_ack_ns']-collects[i]['pause_ack_ns'])/1e9,
   'resident_gpa_pages_before_attribution':resident_gpa_count(reading,slots) if slots else None,
   'right_censored_after_last_tool':i==len(paths)-1}
  cp=path/'compression.json';comp=None;resident=None;sample_rows=None
  census=path/'resident-census.json';sample_path=path/'resident-sample.jsonl.gz'
  if cp.exists():
   comp=json.loads(cp.read_text());inputs[str(cp)]=digest(cp)
   row['compression']=compress_model(comp,0)
  if census.exists():
   resident=json.loads(census.read_text());inputs[str(census)]=digest(census)
  elif comp:resident={k:comp[k] for k in ('resident_population','sample_design')}
  if resident:
   if sample_path.exists():
    sample_rows=[json.loads(l) for l in gzip.open(sample_path,'rt') if l.strip()]
    inputs[str(sample_path)]=digest(sample_path)
   elif comp:sample_rows=comp['samples']
   else:raise ValueError('resident census lacks attributed sample')
   row['resident_population']=resident['resident_population']
   row['resident_sample_model']=resident_model(resident,sample_rows,0)
  elif not allow_missing:raise ValueError('missing resident-census.json: '+str(path))
  if previous is not None:
   prev_i,prev_pages,prev_comp,prev_row,prev_resident,prev_samples=previous
   gap_ns=arms[i]['begin_ns']-collects[prev_i]['resume_ack_ns']
   if gap_ns<0:raise ValueError('negative interstep gap')
   gap=gap_ns/1e9
   g={'from_step':paths[prev_i].name,'to_step':path.name,'from_group_id':prev_row['group_id'],'to_group_id':row['group_id'],
    'gap_seconds':gap,'gap_begin_ns':collects[prev_i]['resume_ack_ns'],'gap_end_ns':arms[i]['begin_ns'],
    'gap_semantics':'observed non-tool interval after collector resume and before next arm request; includes model/network/harness and guest background execution, not pure reasoning',
    'touched_set_model':transitions(prev_pages,pages,gap)}
   if prev_resident:
    N=prev_resident['sample_design']['population_pages']
    g['unique_host_pfn_left_endpoint_rectangle_byte_seconds']=N*PAGE*gap
    pop=prev_resident['resident_population']
    rss=pop.get('pre_capture_vmm_rss_kib',pop.get('pre_capture_vmm_smaps_rollup',{}).get('Rss_kib'))
    if rss is not None:g['vmm_rss_left_endpoint_rectangle_byte_seconds']=rss*1024*gap
    g['resident_sample_model']=resident_model(prev_resident,prev_samples,gap)
    current_count=pop.get('resident_gpa_pages');next_count=group_arms.get(row['group_id'],{}).get('resident_pages_at_arm')
    g['endpoint_resident_gpa_drift']={'collect_pre_attribution':current_count,'next_arm':next_count,
     'difference_pages':None if current_count is None or next_count is None else next_count-current_count,
     'scope':'includes collector-induced residency and guest background changes; not pure workload growth'}
   if prev_comp:g['compression']=compress_model(prev_comp,gap)
   if prev_row['resident_gpa_pages_before_attribution'] is not None:
    g['resident_gpa_left_endpoint_rectangle_byte_seconds']=prev_row['resident_gpa_pages_before_attribution']*PAGE*gap
   gaps.append(g)
  perstep.append(row);previous=(i,pages,comp,row,resident,sample_rows)
 total={'complete_gaps':len(gaps),'sum_gap_seconds':sum(g['gap_seconds'] for g in gaps),
  'sum_arm_pause_seconds':sum(s['arm_pause_seconds'] for s in perstep),'sum_collect_pause_seconds':sum(s['collect_pause_seconds'] for s in perstep)}
 for field in ('unique_host_pfn_left_endpoint_rectangle_byte_seconds','vmm_rss_left_endpoint_rectangle_byte_seconds','resident_gpa_left_endpoint_rectangle_byte_seconds'):
  present=[g[field] for g in gaps if field in g]
  total[field]=sum(present) if len(present)==len(gaps) and gaps else None
 for field in ('instantaneous_static_gap_saved_byte_seconds','exclusive_mapping_sample_static_gap_saved_byte_seconds','codec_only_all_at_once_illustration_saved_byte_seconds'):
  present=[g['compression'][field] for g in gaps if 'compression' in g]
  total[field]=sum(present) if len(present)==len(gaps) and gaps else None
 category_areas=collections.Counter()
 for g in gaps:
  for c,v in g.get('resident_sample_model',{}).get('categories',{}).items():category_areas[c]+=v['left_endpoint_rectangle_byte_seconds']
 total['resident_category_estimated_rectangle_byte_seconds']=dict(category_areas)
 total['compression_measured']=any('compression' in row for row in perstep)
 result={'schema':'aries.attribution-opportunity.v1','source':str(root),'vm_identity':list(next(iter(identity))),'steps':perstep,'gaps':gaps,'totals':total,
  'limits':['Opportunity models, not measured memory reclamation or speedup','Boundary touched sets are a subset of resident RAM; uniform resident sample uses distinct host-PFN population separately','Background accesses/mutations during gaps unobserved; constant-content/persistence assumption unverified','Last capture right-censored, no final gap invented','Separate alternative models must not be added together','Cross-step GPA reuse does not prove allocation/content identity','Sampling intervals omit systematic uncertainty; correlated windows are not independent task repetitions','One execution per workload cannot establish broad generality']}
 (output/'opportunity.json').write_text(json.dumps(result,indent=2)+'\n')
 (output/'input-hashes.json').write_text(json.dumps(inputs,indent=2)+'\n')
 return result

if __name__=='__main__':
 p=argparse.ArgumentParser(description=__doc__);p.add_argument('--attribution-root',required=True);p.add_argument('--output',required=True);p.add_argument('--allow-missing-compression',action='store_true',help='Legacy timing-only mode: allow missing resident census/sample; current metadata-only runs do not need this flag')
 a=p.parse_args();r=analyze(a.attribution_root,a.output,a.allow_missing_compression);print(json.dumps(r['totals'],indent=2))
