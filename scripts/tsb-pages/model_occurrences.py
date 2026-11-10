"""Strict occurrence pairing for the separately versioned two-rerun extension."""
import hashlib,json,re
class OccurrenceError(ValueError):pass
def need(ok,message):
 if not ok:raise OccurrenceError(message)
def record_hash(row):
 return hashlib.sha256(json.dumps(row,sort_keys=True,separators=(',',':'),ensure_ascii=False).encode()).hexdigest()
def pair_model_occurrences(events,source_sha256):
 need(isinstance(source_sha256,str) and re.fullmatch('[0-9a-f]{64}',source_sha256),'raw source SHA256 required')
 pending_batches=set();completed_batches=set();last_batch=None;active={};seen=set();pairs=[];sequence=0;session=None;last_end_ns=None
 for index,row in enumerate(events):
  event=row.get('event')
  if event=='batch_requested':
   bid=row.get('batch_id');need(isinstance(bid,str) and bid and bid not in pending_batches|completed_batches,'duplicate/invalid batch declaration')
   pending_batches.add(bid)
  elif event=='batch_completed':
   bid=row.get('batch_id');need(bid in pending_batches,'batch completion without declaration')
   pending_batches.remove(bid);completed_batches.add(bid);last_batch=bid
  if event not in ('model_call_started','model_call_ended'):continue
  oid=row.get('model_occurrence_id');need(isinstance(oid,str) and re.fullmatch(r'model-occurrence-[0-9]+',oid),'model occurrence identity required')
  need('model_gap_after_batch_id' in row,'explicit model gap identity required')
  native=(row.get('run_id'),row.get('call_id'))
  need(all(isinstance(v,str) and v for v in native),'raw native run/call identity required')
  hierarchy=(row.get('session_id'),row.get('turn_id'))
  need(all(isinstance(v,str) and v for v in hierarchy),'session hierarchy required')
  if session is None:session=hierarchy
  need(hierarchy==session,'mixed observation sessions')
  stamp=int(row['plugin_monotonic_ns']);need(stamp>=0,'negative callback time')
  if event=='model_call_started':
   need(not pending_batches,'model started during unfinished tool batch')
   need(not active,'duplicate active native call or overlapping provider occurrences')
   need(last_end_ns is None or stamp>=last_end_ns,'provider callback intervals overlap or reverse')
   need(oid not in seen,'reused occurrence identity')
   sequence+=1;need(int(oid.rsplit('-',1)[1])==sequence,'nonmonotonic occurrence sequence')
   need(row['model_gap_after_batch_id']==last_batch,'model gap differs from latest completed batch')
   seen.add(oid);active[oid]=(index,row,native)
  else:
   need(oid in active,'model end without matching occurrence start')
   start_index,start,start_native=active.pop(oid)
   need(native==start_native,'native identity changed within occurrence')
   need(row['model_gap_after_batch_id']==start['model_gap_after_batch_id'],'gap changed within occurrence')
   need(row.get('plugin_pid')==start.get('plugin_pid'),'source process clock changed')
   need(int(start['plugin_monotonic_ns'])<=stamp,'negative callback interval')
   last_end_ns=stamp
   duration=row.get('durationMs');need(isinstance(duration,(int,float)) and not isinstance(duration,bool) and duration>=0,'invalid native provider duration')
   pairs.append({'model_occurrence_id':oid,'run_id':native[0],'call_id':native[1],'model_gap_after_batch_id':start['model_gap_after_batch_id'],'start':start,'end':row,'source_sha256':source_sha256,'start_event_index':start_index,'end_event_index':index,'start_record_sha256':record_hash(start),'end_record_sha256':record_hash(row),'record_hash_encoding':'UTF-8 canonical sorted-key compact JSON; raw source SHA binds original serialized bytes'})
 need(not active,'unfinished provider occurrence')
 need(bool(pairs),'no complete provider occurrences')
 return pairs
def assign_occurrences_to_gaps(pairs,gaps,tools):
 need(len(gaps)==len(tools)+1,'tool/gap phase count mismatch')
 mapping={None:(0,gaps[0])}
 for index,tool in enumerate(tools):
  bid=tool['group_id'];need(bid not in mapping,'duplicate tool group')
  mapping[bid]=(index+1,gaps[index+1])
 assigned=[];covered=set()
 for pair in pairs:
  key=pair['model_gap_after_batch_id'];need(key in mapping,'provider gap is outside measured phase set')
  i,gap=mapping[key];covered.add(i);assigned.append((i,gap,pair))
 need(covered==set(range(len(gaps))),'measured gap lacks a provider occurrence')
 need([a[0] for a in assigned]==sorted(a[0] for a in assigned),'provider gap order reversed')
 return assigned
