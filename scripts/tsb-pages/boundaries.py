#!/usr/bin/env python3
import argparse,collections,datetime,hashlib,json,os,re
from pathlib import Path
ap=argparse.ArgumentParser();ap.add_argument('--run',type=Path,required=True);ap.add_argument('--observer-run',type=Path,required=True);ap.add_argument('--task-id',required=True);a=ap.parse_args();os.umask(0o077)
r=a.run.resolve();out=a.observer_run.resolve();h=r/a.task_id/'harness';sources={}
def text(p):
 b=p.read_bytes();sources[str(p.resolve())]={'sha256':hashlib.sha256(b).hexdigest(),'bytes':len(b)};return b.decode()
def rows(p):return [json.loads(l) for l in text(p).splitlines() if l.strip()]
def ms(t):return datetime.datetime.fromisoformat(t.replace('Z','+00:00')).timestamp()*1000
plugin=rows(h/'telemetry/aries-step-pages.jsonl');bridge=rows(r/a.task_id/'bridge/tool-calls.jsonl');observations=rows(out/'observer/steps.jsonl');errors=rows(out/'observer/errors.jsonl')
group_path=out/'observer/groups.jsonl'
group_rows=rows(group_path) if group_path.exists() else []
groups=[g for g in group_rows if g.get('event')=='group_end']
llm_batch_mode=any(g.get('event')=='batch_begin' for g in group_rows)
batch_starts={g['group_id']:g for g in group_rows if g.get('event')=='batch_begin'}
group_mode=group_path.exists()
group_for_call={}
for group in groups:
 for cid in group['member_tool_call_ids']:
  group_for_call.setdefault(cid,[]).append(group)
messages=[]
for p in sorted((h/'telemetry').glob('*.jsonl')):
 if 'trajectory' in p.name or p.name=='aries-step-pages.jsonl':continue
 for o in rows(p):
  if o.get('type')=='message':messages.append(o)
model=[]
for line in text(h/'gateway.log').splitlines():
 if '[model-fetch] start' in line:
  stamp=line.split()[0];provider=re.search(r'provider=(\S+)',line);name=re.search(r'model=(\S+)',line)
  model.append({'timestamp':stamp,'timestamp_ms':ms(stamp),'provider':provider.group(1) if provider else None,'model':name.group(1) if name else None})
config=json.loads(text(h/'openclaw.json'));result=json.loads(text(r/'run-result.json'))
assistant=[];native_results={};toolcalls=[]
for o in messages:
 m=o.get('message',{})
 if m.get('role')=='assistant':
  calls=[{'id':c.get('id'),'name':c.get('name')} for c in m.get('content',[]) if isinstance(c,dict) and c.get('type')=='toolCall'];toolcalls.extend(calls)
  assistant.append({'message_id':o.get('id'),'persisted_at':o.get('timestamp'),'provider':m.get('provider'),'model':m.get('model'),'stop_reason':m.get('stopReason'),'tool_calls':calls,'usage':m.get('usage')})
 if m.get('role')=='toolResult':native_results.setdefault(m.get('toolCallId'),[]).append({'persisted_at':o.get('timestamp'),'is_error':m.get('isError',False),'native_status':m.get('details',{}).get('status'),'failure_kind':m.get('details',{}).get('failureKind'),'native_duration_ms':m.get('details',{}).get('durationMs')})
checks=[]
def check(name,ok,detail=None):checks.append({'name':name,'passed':bool(ok),'detail':detail})
# New plugin telemetry uses occurrence identities; legacy telemetry remains
# audited by its original protocol. Never collapse repeated native IDs.
model_occurrence_pairs=[]
if any('model_occurrence_id' in event for event in plugin):
 from model_occurrences import pair_model_occurrences
 helper=Path(__file__).with_name('model_occurrences.py')
 text(helper)
 try:
  model_occurrence_pairs=pair_model_occurrences(plugin,sources[str((h/'telemetry/aries-step-pages.jsonl').resolve())]['sha256'])
  check('model_occurrences_complete_and_gap_bound',True,len(model_occurrence_pairs))
 except (ValueError,KeyError,TypeError) as error:
  check('model_occurrences_complete_and_gap_bound',False,str(error))

check('native_call_ids_unique_for_current_audit',len({c['id'] for c in toolcalls})==len(toolcalls),'Collector retains batch-scoped identities; this audit requires unambiguous native call IDs within this session.')
check('zero_observer_errors',not errors,len(errors));check('zero_plugin_measurement_errors',not [e for e in plugin if e['event']=='measurement_error'])
byid=collections.defaultdict(list)
for e in plugin:
 if e.get('tool_call_id'):byid[e['tool_call_id']].append(e)

def registration_censor_candidate(result,task_id,group_rows,groups,errors,plugin,observations,toolcalls,native_results,model):
 """Recognize only an uncollected terminal declaration, never an executed tool."""
 tasks=[task for task in result.get('tasks',[]) if task.get('task_id')==task_id]
 if len(tasks)!=1:return None
 task=tasks[0];harness=task.get('harness',{});isolation=task.get('isolation',{})
 if not (harness.get('status')=='canceled' and 'context deadline exceeded' in str(task.get('error',''))+' '+str(harness.get('error','')) and task.get('cleanup',{}).get('status')=='succeeded' and isolation.get('status')=='confirmed' and isolation.get('harness_stopped') is True and isolation.get('bridge_revoked') is True):return None
 if len(errors)!=1 or errors[0].get('error')!='shutdown with incomplete LLM batch':return None
 if any(event.get('event')=='measurement_error' for event in plugin):return None
 starts=[row for row in group_rows if row.get('event')=='batch_begin']
 if not starts or not groups or len(starts)!=len(groups)+1 or group_rows[-1] is not starts[-1]:return None
 batch=starts[-1];gid=batch.get('group_id');ids=batch.get('expected_tool_call_ids')
 if not isinstance(gid,str) or not gid or batch.get('batch_id')!=gid or batch.get('llm_step_id')!=gid:return None
 if not isinstance(ids,list) or not ids or not all(isinstance(cid,str) and cid for cid in ids) or len(ids)!=len(set(ids)):return None
 start_ids=[row.get('group_id') for row in starts];end_ids=[row.get('group_id') for row in groups]
 if len(set(start_ids))!=len(start_ids) or len(set(end_ids))!=len(end_ids) or start_ids[:-1]!=end_ids:return None
 tools=batch.get('requested_tools',[])
 if not isinstance(tools,list) or any(not isinstance(tool,dict) for tool in tools):return None
 if [tool.get('tool_call_id') for tool in tools if isinstance(tool,dict)]!=ids or any(not isinstance(tool.get('tool_name'),str) or not tool['tool_name'] for tool in tools):return None
 error=errors[0];metadata=error.get('metadata',{})
 identity=['session_id','turn_id','batch_id','llm_step_id','expected_tool_call_ids','requested_tools','native_session_key','native_assistant_message_id','native_assistant_timestamp_ms','native_assistant_provider','native_assistant_model','native_assistant_stop_reason','plugin_pid','plugin_timestamp_ms']
 if any(metadata.get(key)!=batch.get(key) for key in identity):return None
 if error.get('completed_tool_call_ids')!=[] or error.get('missing_tool_call_ids')!=ids:return None
 if batch.get('native_assistant_stop_reason')!='toolUse':return None
 if not any(event.get('session_id')==batch.get('session_id') and event.get('turn_id')==batch.get('turn_id') for event in plugin):return None
 declaration_ms=batch.get('plugin_timestamp_ms');assistant_ms=batch.get('native_assistant_timestamp_ms')
 if not isinstance(declaration_ms,(int,float)) or not isinstance(assistant_ms,(int,float)) or assistant_ms>declaration_ms:return None
 if not model or any(request['timestamp_ms']>declaration_ms for request in model):return None
 arm=batch.get('observer_arm',{});arm_begin=arm.get('arm_begin_ns');arm_end=arm.get('arm_end_ns')
 if not isinstance(arm_begin,int) or not isinstance(arm_end,int) or not 0<arm_begin<=arm_end or batch.get('host_monotonic_ns')!=arm_end:return None
 ack=batch.get('observer_ack',{})
 if not (ack.get('ok') is True and ack.get('collection')=='armed' and ack.get('batch_id')==gid and ack.get('remaining_expected_tool_call_ids')==ids and ack.get('remaining_active_tool_call_ids')==[]):return None
 expected=set(ids)
 if expected.intersection(call.get('id') for call in toolcalls) or any(native_results.get(cid) for cid in ids):return None
 if any(event.get('tool_call_id') in expected for event in plugin+observations):return None
 final_plugin=[event for event in plugin if event.get('batch_id')==gid]
 if any(event.get('event') not in ('batch_requested','batch_confirmed') for event in final_plugin):return None
 if any(sum(event.get('event')==name for event in final_plugin)>1 for name in ('batch_requested','batch_confirmed')):return None
 if any(event.get('expected_tool_call_ids')!=ids for event in final_plugin):return None
 return {'group_id':gid,'censoring_kind':'registration_request_without_native_persistence',
         'expected_tool_call_ids':ids,'completed_tool_call_ids':[],'missing_tool_call_ids':ids,
         'native_assistant_persistence_confirmed':False,'native_tool_execution_confirmed':False,
         'plugin_registration_confirmation_observed':any(event.get('event')=='batch_confirmed' for event in final_plugin),
         'observer_arm_completed':True,'group_end_present':False,'comparison_reading_available':False,
         'declaration_plugin_timestamp_ms':declaration_ms,'native_assistant_timestamp_ms':assistant_ms,
         'last_model_request_timestamp_ms':max(request['timestamp_ms'] for request in model),
         'shutdown_error':error,'harness_status':harness['status'],'harness_duration_ns':harness.get('duration'),
         'evidence_limit':'The observer received and armed a terminal assistant-request declaration; harvested native persistence and tool execution are unconfirmed. No final footprint exists.'}

deadline_tasks=[task for task in result.get('tasks',[]) if task.get('task_id')==a.task_id and task.get('harness',{}).get('status')=='canceled' and 'context deadline exceeded' in str(task.get('error',''))+' '+str(task.get('harness',{}).get('error',''))]
censor_candidate_group=None;censor_candidate_calls=[]
if llm_batch_mode and groups and deadline_tasks and not errors and not [event for event in plugin if event.get('event')=='measurement_error']:
 last_group=groups[-1];last_ids=last_group.get('expected_tool_call_ids',[])
 requested=[event for cid in last_ids for event in byid[cid] if event.get('event')=='end_requested']
 missing=[cid for cid in last_ids if not [event for event in byid[cid] if event.get('event')=='end_confirmed'] and not native_results.get(cid)]
 valid_counts=bool(last_ids) and all(
  len([event for event in byid[cid] if event.get('event')=='end_requested'])==1 and
  len([event for event in byid[cid] if event.get('event')=='end_confirmed'])==(0 if cid in missing else 1) and
  len(native_results.get(cid,[]))==(0 if cid in missing else 1) and
  len([event for event in byid[cid] if event.get('event')=='begin_requested'])==len([event for event in byid[cid] if event.get('event')=='begin_confirmed'])<=1
  for cid in last_ids)
 if missing and valid_counts and last_group.get('measurement_valid') is True and last_group.get('completed_tool_call_ids')==last_ids and requested and not any(request['timestamp_ms']>max(event['plugin_timestamp_ms'] for event in requested) for request in model):
  censor_candidate_group=last_group['group_id'];censor_candidate_calls=missing

uncollected_censor=registration_censor_candidate(result,a.task_id,group_rows,groups,errors,plugin,observations,toolcalls,native_results,model) if llm_batch_mode else None
if uncollected_censor and censor_candidate_group is None:
 censor_candidate_group=uncollected_censor['group_id'];censor_candidate_calls=uncollected_censor['expected_tool_call_ids']

observed={e['tool_call_id']:e for e in observations if e['event']=='step_end'}
steps=[];assignment=collections.Counter()
for call in toolcalls:
 id=call['id'];events=byid[id];events_by_type=collections.defaultdict(list)
 for e in events:events_by_type[e['event']].append(e)
 has_begin=bool(events_by_type['begin_requested'] or events_by_type['begin_confirmed'])
 required=['end_requested','end_confirmed']+(['begin_requested','begin_confirmed'] if has_begin or not llm_batch_mode else [])
 paired=all(len(events_by_type[k])==1 for k in required)
 check('paired_native_result_and_optional_admission:'+id if llm_batch_mode else 'paired_native_step:'+id,paired)
 if not paired:continue
 begin=events_by_type['begin_confirmed'][0] if has_begin else None
 end_req=events_by_type['end_requested'][0];end=events_by_type['end_confirmed'][0]
 ops=[]
 for operation in bridge:
  received=operation['timing']['received_at'];finished=operation['timestamp']
  if begin and begin['plugin_timestamp_ms']<=ms(received)<=end_req['plugin_timestamp_ms']:
   assignment[operation['sequence']]+=1
   ops.append({k:operation.get(k) for k in ['sequence','operation_class','timestamp','duration_ms','exit_code','status','command_hash']}|{'received_at':received,'begin_inside_step':True,'end_inside_step':ms(finished)<=end_req['plugin_timestamp_ms']})
 result_rows=native_results.get(id,[]);check('one_native_result:'+id,len(result_rows)==1)
 next_model=next((m for m in model if m['timestamp_ms']>end_req['plugin_timestamp_ms']),None)
 result_after=len(result_rows)==1 and ms(result_rows[0]['persisted_at'])>=end['plugin_timestamp_ms']
 check('end_ack_before_native_result:'+id,result_after)
 check('end_ack_before_next_model:'+id,next_model is not None and next_model['timestamp_ms']>=end['plugin_timestamp_ms'])
 observation=observed.get(id)
 call_groups=group_for_call.get(id,[])
 reading=(call_groups[0].get('reading',{}) if len(call_groups)==1 else {}) if group_mode else (observation.get('reading',{}) if observation else {})
 check('one_valid_observation:'+id,observation is not None and observation.get('measurement_valid') is True and (not group_mode or (len(call_groups)==1 and call_groups[0].get('measurement_valid') is True)))

 steps.append({'tool_call_id':id,'tool_name':call['name'],'begin_observed':has_begin,'plugin_events':events,'bridge_operations':ops,'bridge_operation_count':len(ops),'activity_classification':'contains sandbox operations' if ops else 'no sandbox operation; VM background activity during native tool interval','native_results':result_rows,'next_model_request':next_model,'observer_window':observation.get('result') if observation else None,'group_id':observation.get('group_id') if observation else None,'operation_membership_scope':'temporal membership; overlapping calls can share operations','observed_accessed_pages':len(reading['accessed_gpas']) if reading and not group_mode else None,'unknown_pages':len(reading['unknown_gpas']) if reading and not group_mode else None})
if not llm_batch_mode:
 check('all_bridge_operations_have_temporal_membership' if group_mode else 'all_bridge_operations_assigned_exactly_once',all(assignment[o['sequence']]>=1 if group_mode else assignment[o['sequence']]==1 for o in bridge),dict(assignment))
group_checks=[];group_windows=[]
if group_mode:
 group_assignment=collections.Counter()
 for group in groups:
  members=group['member_tool_call_ids']
  ev=[event for cid in members for event in byid[cid]]
  starts=[x['plugin_timestamp_ms'] for x in ev if x['event']=='begin_confirmed']
  ends=[x['plugin_timestamp_ms'] for x in ev if x['event']=='end_requested']
  acks=[x['plugin_timestamp_ms'] for x in ev if x['event']=='end_confirmed']
  paired=len(ends)==len(acks)==len(members) and (llm_batch_mode or len(starts)==len(members))
  check('group_members_paired:'+group['group_id'],paired)
  if not ends:continue
  window_start=min(starts) if starts else None
  if llm_batch_mode:
   gid=group['group_id']
   registrations=[e for e in plugin if e.get('event')=='batch_confirmed' and e.get('batch_id',e.get('llm_step_id',e.get('group_id')))==gid]
   check('one_batch_registration_ack:'+gid,len(registrations)==1)
   batch=batch_starts.get(gid,{})
   expected=batch.get('expected_tool_call_ids',[])
   matches=[m for m in assistant if [c['id'] for c in m['tool_calls']]==expected and expected]
   check('one_complete_native_assistant_request:'+gid,len(matches)==1)
   check('batch_expected_equals_completed:'+gid,expected==group.get('expected_tool_call_ids') and sorted(expected)==sorted(members)==sorted(group.get('completed_tool_call_ids',[])) and len(expected)==len(set(expected)))
   if registrations:
    window_start=registrations[0]['plugin_timestamp_ms']
    check('batch_armed_before_native_admissions:'+gid,all(window_start<=t for t in starts))
   else:window_start=None
   declared={m['tool_call_id']:m.get('begin_observed') for m in group.get('group_members',[])}
   check('admission_presence_matches_group_members:'+gid,all(declared.get(cid)==bool([e for e in byid[cid] if e['event']=='begin_confirmed']) for cid in members))
  collection_ms=group['collection_complete_wall_time_ns']/1e6
  next_model=next((m for m in model if m['timestamp_ms']>max(ends)),None)
  check('group_collected_before_next_model:'+group['group_id'],next_model is not None and bool(acks) and paired and collection_ms<=next_model['timestamp_ms'] and max(acks)<=next_model['timestamp_ms'])
  check('one_collected_ack_per_group:'+group['group_id'],sum(observed.get(cid,{}).get('observer_ack',{}).get('collection')=='collected' for cid in members)==1)
  check('no_interior_reset_gaps:'+group['group_id'],group['reading'].get('transition_gaps')==[])
  if window_start is not None:
   group_windows.append({'group_id':group['group_id'],'member_tool_call_ids':members,'start_ms':window_start,'result_request_end_ms':max(ends),'delivery_end_ms':max(ends+acks) if group['group_id']==censor_candidate_group else max(acks) if acks else max(ends),'collection_complete_ms':collection_ms,'delivery_censored':group['group_id']==censor_candidate_group})
  for op in bridge:
   if window_start is not None and window_start<=ms(op['timing']['received_at'])<=(max(ends+acks) if group['group_id']==censor_candidate_group else max(acks) if acks else max(ends)):
    group_assignment[op['sequence']]+=1
  group_checks.append({'group_id':group['group_id'],'protocol_complete':paired,'delivery_censored':group['group_id']==censor_candidate_group,'member_tool_call_ids':members,'concurrency_peak':group.get('concurrency_peak'),'collection_complete_wall_time_ns':group['collection_complete_wall_time_ns'],'next_model_request':next_model,'observed_accessed_pages':(group['reading']['observed_pages'] if 'observed_pages' in group['reading'] else len(group['reading']['accessed_gpas'])),'unknown_pages':(group['reading']['unknown_pages'] if 'unknown_pages' in group['reading'] else len(group['reading']['unknown_gpas']))})
 check('one_group_per_native_call',set(group_for_call)=={c['id'] for c in toolcalls} and all(len(gs)==1 for gs in group_for_call.values()))
 check('all_bridge_operation_starts_in_exactly_one_group',all(group_assignment[o['sequence']]==1 for o in bridge),dict(group_assignment))


def interval_parts(start,end,windows,end_key):
 overlaps=[];covered=[]
 for window in windows:
  lo=max(start,window['start_ms']);hi=min(end,window[end_key])
  if lo<hi:
   overlaps.append({'group_id':window['group_id'],'start_timestamp_ms':lo,'end_timestamp_ms':hi,'duration_ms':hi-lo})
   covered.append((lo,hi))
 outside=[];cursor=start
 for lo,hi in sorted(covered):
  if lo>cursor:outside.append({'start_timestamp_ms':cursor,'end_timestamp_ms':lo,'duration_ms':lo-cursor})
  cursor=max(cursor,hi)
 if cursor<end:outside.append({'start_timestamp_ms':cursor,'end_timestamp_ms':end,'duration_ms':end-cursor})
 return overlaps,outside

operation_intervals=[]
for operation in (bridge if group_mode else []):
 started=ms(operation['timing']['received_at']);finished=ms(operation['timestamp'])
 origins=[window for window in group_windows if window['start_ms']<=started<=window['delivery_end_ms']]
 overlaps,outside=interval_parts(started,finished,group_windows,'delivery_end_ms')
 ready_overlaps,outside_ready=interval_parts(started,finished,group_windows,'result_request_end_ms')
 origin=origins[0] if len(origins)==1 else None
 yielded=bool(origin and any(item.get('native_status')=='running' for cid in origin['member_tool_call_ids'] for item in native_results.get(cid,[])))
 operation_intervals.append({
  **{key:operation.get(key) for key in ['sequence','operation_class','status','exit_code','duration_ms','command_hash']},
  'received_at':operation['timing']['received_at'],'finished_at':operation['timestamp'],
  'start_group_ids':[window['group_id'] for window in origins],
  'overlapping_group_ids':[item['group_id'] for item in overlaps],
  'group_interval_overlaps':overlaps,'outside_llm_delivery_intervals':outside,
  'outside_llm_delivery_duration_ms':sum(item['duration_ms'] for item in outside),
  'outside_result_ready_intervals':outside_ready,
  'outside_result_ready_duration_ms':sum(item['duration_ms'] for item in outside_ready),
  'end_after_origin_result_request':bool(origin and finished>origin['result_request_end_ms']),
  'end_after_origin_delivery':bool(origin and finished>origin['delivery_end_ms']),
  'origin_group_has_native_running_result':yielded,
  'transport_record_terminal':operation.get('status') in ('completed','failed','canceled'),
  'command_exit_confirmed':operation.get('status') in ('completed','failed'),
  'interpretation':'temporal overlap only; no per-operation page attribution; canceled transport does not confirm guest command termination',
 })
asynchronous_operations=[operation for operation in operation_intervals if operation['end_after_origin_result_request'] or len(operation['overlapping_group_ids'])>1 or operation['status']=='canceled']
if group_mode:
 check('all_bridge_interval_timestamps_ordered',all(ms(operation['timing']['received_at'])<=ms(operation['timestamp']) for operation in bridge))

check('all_observed_ids_are_native',set(observed)=={c['id'] for c in toolcalls})
check('all_bridge_operations_have_terminal_transport_records',all(o.get('status') in ('completed','failed','canceled') and isinstance(o.get('exit_code'),int) for o in bridge))
usage=collections.Counter()
for message in assistant:
 for k,v in (message.get('usage') or {}).items():
  if isinstance(v,(int,float)):usage[k]+=v
resolved_defaults=config['agents']['defaults'].get('models')
if any(m['model']=='deepseek-flash' for m in assistant):
 params=(resolved_defaults or {}).get('aries/deepseek-flash',{}).get('params',{})
 check('configured_flash_high_thinking',params.get('thinking')=='high' and params.get('extra_body',{}).get('reasoning_effort')=='high' and params.get('extra_body',{}).get('thinking',{}).get('type')=='enabled')
 check('native_response_model_flash',all(m['model']=='deepseek-flash' for m in assistant))

failed_checks=[entry for entry in checks if not entry['passed']]
allowed_censor_failures={'paired_native_result_and_optional_admission:'+cid for cid in censor_candidate_calls}
if censor_candidate_group:
 allowed_censor_failures.update({'group_members_paired:'+censor_candidate_group,'group_collected_before_next_model:'+censor_candidate_group})
if uncollected_censor:allowed_censor_failures={'zero_observer_errors'}
audit_status='passed' if not failed_checks else 'censored' if censor_candidate_group and all(entry['name'] in allowed_censor_failures for entry in failed_checks) else 'failed'
censored_group_ids=[censor_candidate_group] if audit_status=='censored' else []
complete_group_ids=[group['group_id'] for group in groups if group['group_id'] not in censored_group_ids and all(len([event for event in byid[cid] if event.get('event')=='end_confirmed'])==1 and len(native_results.get(cid,[]))==1 for cid in group['member_tool_call_ids'])]

output={'schema':'aries.native-step-boundary-audit.v4','postprocessor_version':'4.1','postprocessor_source_sha256':hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),'postprocessor_source_path':str(Path(__file__).resolve()),'group_windows':group_windows,'operation_intervals':operation_intervals,'asynchronous_bridge_operations':asynchronous_operations,'operation_interval_summary':{'operations':len(operation_intervals),'spanning_result_ready_boundary':sum(operation['end_after_origin_result_request'] for operation in operation_intervals),'spanning_delivery_boundary':sum(operation['end_after_origin_delivery'] for operation in operation_intervals),'overlapping_multiple_groups':sum(len(operation['overlapping_group_ids'])>1 for operation in operation_intervals),'with_exterior_intervals':sum(bool(operation['outside_llm_delivery_intervals']) for operation in operation_intervals),'canceled_transport_records':sum(operation['status']=='canceled' for operation in operation_intervals)},'measurement_granularity':'llm_reasoning_step' if llm_batch_mode else 'busy_period_group' if group_mode else 'tool_call','groups':group_checks,'ambiguous_operation_memberships':{str(k):v for k,v in assignment.items() if v>1},'status':audit_status,'censored_group_ids':censored_group_ids,'complete_group_ids':complete_group_ids,'primary_metric_group_ids':complete_group_ids if audit_status in ('passed','censored') else [],'censored_native_call_ids':censor_candidate_calls if audit_status=='censored' else [],'censored_groups_without_comparison':[uncollected_censor['group_id']] if audit_status=='censored' and uncollected_censor else [],'terminal_uncollected_batch':uncollected_censor,'censoring_reason':('native context deadline left a final observer declaration without confirmed native persistence or execution; no comparison footprint exists' if uncollected_censor else 'native context deadline interrupted final result persistence/acknowledgement; the final raw reading is not a completed response-to-LLM window') if audit_status=='censored' else None,'run_root':str(r),'run_summary':result.get('summary'),'task_outcomes':[{'task_id':t.get('task_id'),'harness_status':t.get('harness',{}).get('status'),'isolation':t.get('isolation'),'evaluation':{k:t.get('evaluation',{}).get(k) for k in ['status','score','reward','verifier_status']},'cleanup':t.get('cleanup')} for t in result.get('tasks',[])],'observer_root':str(out/'observer'),'scope':'native assistant tool-request membership, call/result protocol, shared LLM-window closure before next reasoning request in batch mode; not unique per-tool causal attribution or complete page coverage','sessions':sorted({e['session_id'] for e in plugin if 'session_id'in e}),'turns':sorted({e['turn_id'] for e in plugin if 'turn_id'in e}),'plugin_pids':sorted({e['plugin_pid'] for e in plugin if 'plugin_pid'in e}),'service_registrations':[e['registration'] for e in plugin if e['event']=='service_started'],'hook_registrations':sorted({e['registration'] for e in plugin if e['event']=='begin_requested'}),'steps':steps,'model_requests':model,'model_occurrence_pairs':model_occurrence_pairs,'native_assistant_messages':assistant,'native_usage_totals':dict(usage),'resolved_model_defaults':config['agents']['defaults'].get('models'),'checks':checks,'sources':sources,'notes':['Bridge timestamps use worker UTC; plugin wall-clock bounds have millisecond granularity. Observed boundaries have tens of milliseconds margin in the deterministic mock.','Observer monotonic bounds are retained separately; native tool-result timestamp represents persistence after synchronous observer acknowledgement.','No command/result text or credentials are duplicated in this audit.','A native tool call with zero bridge operations remains a Step; its observed VM pages are not causally attributed to an unexecuted sandbox command.','Bridge operation admission must belong to one LLM delivery window; a background operation may continue across result delivery, later windows, or model-reasoning gaps.','Delivery windows run from batch registration confirmation through final result-persistence acknowledgement; result-ready hook times and collector timestamps are retained separately.','Operation intervals show temporal overlap only, not which operation caused a page access. Canceled transport is a recorded outcome, not confirmation of guest command termination.','Background descendant ownership and unreadable page accuracy are not certified by this boundary audit.']}
(out/'boundary-audit.json').write_text(json.dumps(output,indent=2)+'\n')
lines=['# LLM-window and native-call boundary audit','',f"Status: **{output['status']}**. {len(steps)} native tool calls; {len(bridge)} constituent bridge operations.",'',output['scope']+'.','',f"Plugin PID(s): {output['plugin_pids']}; service registration(s): {output['service_registrations']}; hook registration(s): {output['hook_registrations']}.",'','| Step | Operations | Accessed pages | Unknown pages | Next model request |','|---|---:|---:|---:|---|']
for s in steps:lines.append(f"| {s['tool_call_id']} | {s['bridge_operation_count']} | {s['observed_accessed_pages']} | {s['unknown_pages']} | {s['next_model_request']['timestamp'] if s['next_model_request'] else 'missing'} |")
if censored_group_ids:lines+=['',f"Native deadline censored final group(s): {censored_group_ids}. Complete groups: {len(complete_group_ids)}. Failed protocol checks remain explicit; censored groups are excluded from primary metrics."]
if audit_status=='censored' and uncollected_censor:lines+=['','Final batch was armed but has no collected footprint; native assistant persistence and tool execution are unconfirmed. Censored groups without comparison: '+str(output['censored_groups_without_comparison'])+'.']
if llm_batch_mode:lines+=['','Primary unit: one LLM reasoning step, from the complete assistant tool request until all its results are returned. Admission-free immediate errors remain explicit; per-call operation membership is supporting information, while every operation start must belong to one batch delivery window; native background execution may outlive that window.']
if group_mode:lines+=['','Group mode: individual call page counts are unavailable; shared group sets are recorded once in groups.jsonl. Per-call operation memberships may overlap. A tool_result_persist timestamp is result-delivery time, not individual parallel execution-completion time.','',f"Completed shared groups: {len(group_checks)}. Group closure is checked against subsequent model dispatch."]
lines+=['',f"Async/background bridge intervals: {len(asynchronous_operations)}; spans beyond delivery: {output['operation_interval_summary']['spanning_delivery_boundary']}; overlaps multiple LLM windows: {output['operation_interval_summary']['overlapping_multiple_groups']}; canceled transport records: {output['operation_interval_summary']['canceled_transport_records']}.",'','Operation admission is checked independently from duration. Intervals outside result-ready/delivery windows remain explicit; no operation or page count is silently dropped.','',f"Postprocessor v{output['postprocessor_version']}, SHA-256 {output['postprocessor_source_sha256']}.",'','All operation receipt/completion timestamps, plugin barriers, native result persistence timestamps, model dispatch sequence, usage, checks and source SHA-256 hashes are retained in `boundary-audit.json`.','',f"Native reported usage: `{dict(usage)}`.",'','Steps with zero bridge operations remain visible as native tool attempts; their page sets include background VM activity and are not evidence of sandbox command execution.','', 'Failed checks: '+', '.join(c['name'] for c in checks if not c['passed'])+'.' if not all(c['passed'] for c in checks) else 'All boundary checks passed.']
(out/'boundary-audit.md').write_text('\n'.join(lines)+'\n')
for name in ['boundary-audit.json','boundary-audit.md']:(out/name).chmod(0o600)
print(json.dumps({'status':output['status'],'steps':len(steps),'bridge_operations':len(bridge),'failed_checks':[c for c in checks if not c['passed']],'usage':dict(usage)}))
