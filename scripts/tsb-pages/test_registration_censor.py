#!/usr/bin/env python3
import ast,copy,json,unittest
from pathlib import Path
work=Path(__file__).parent
source=work/'boundaries.py'
tree=ast.parse(source.read_text())
helper=next(n for n in tree.body if isinstance(n,ast.FunctionDef) and n.name=='registration_censor_candidate')
scope={};exec(compile(ast.Module(body=[helper],type_ignores=[]),str(source),'exec'),scope)
guard=scope['registration_censor_candidate']
def fixture():
 meta={'session_id':'session','turn_id':'1','batch_id':'step2','llm_step_id':'step2','expected_tool_call_ids':['final'],'requested_tools':[{'tool_call_id':'final','tool_name':'exec'}],'native_session_key':'native-session','native_assistant_message_id':'request','native_assistant_timestamp_ms':90,'native_assistant_provider':'aries','native_assistant_model':'deepseek-flash','native_assistant_stop_reason':'toolUse','plugin_pid':11,'plugin_timestamp_ms':100}
 final={**meta,'event':'batch_begin','group_id':'step2','observer_arm':{'arm_begin_ns':1,'arm_end_ns':2},'host_monotonic_ns':2,'observer_ack':{'ok':True,'collection':'armed','batch_id':'step2','remaining_expected_tool_call_ids':['final'],'remaining_active_tool_call_ids':[]}}
 first={'event':'batch_begin','group_id':'step1'};end={'event':'group_end','group_id':'step1'}
 return {'result':{'tasks':[{'task_id':'task','error':'context deadline exceeded','harness':{'status':'canceled','duration':900000000000},'isolation':{'status':'confirmed','harness_stopped':True,'bridge_revoked':True},'cleanup':{'status':'succeeded'}}]},'task_id':'task','group_rows':[first,end,final],'groups':[end],'errors':[{'error':'shutdown with incomplete LLM batch','metadata':copy.deepcopy(meta),'completed_tool_call_ids':[],'missing_tool_call_ids':['final']}],'plugin':[{'event':'service_started','session_id':'session','turn_id':'1'}],'observations':[],'toolcalls':[{'id':'prior'}],'native_results':{'prior':[{}]},'model':[{'timestamp_ms':80}]}
class GuardTests(unittest.TestCase):
 def reject(self,mutate):
  data=fixture();mutate(data);self.assertIsNone(guard(**data))
 def test_terminal_declaration_without_native_snapshot(self):
  found=guard(**fixture());self.assertEqual(found['group_id'],'step2');self.assertFalse(found['native_tool_execution_confirmed']);self.assertFalse(found['native_assistant_persistence_confirmed']);self.assertFalse(found['comparison_reading_available'])
 def test_does_not_capture_existing_collected_censor(self):self.reject(lambda d:d['errors'].clear())
 def test_optional_plugin_registration_snapshot(self):
  d=fixture();d['plugin'] += [{'event':name,'batch_id':'step2','expected_tool_call_ids':['final']} for name in ['batch_requested','batch_confirmed']];self.assertTrue(guard(**d)['plugin_registration_confirmation_observed'])
 def test_requires_native_deadline(self):self.reject(lambda d:d['result']['tasks'][0].update(error='operator canceled'))
 def test_requires_canceled_harness(self):self.reject(lambda d:d['result']['tasks'][0]['harness'].update(status='failed'))
 def test_requires_cleanup(self):self.reject(lambda d:d['result']['tasks'][0]['cleanup'].update(status='failed'))
 def test_requires_isolation(self):self.reject(lambda d:d['result']['tasks'][0]['isolation'].update(bridge_revoked=False))
 def test_does_not_mask_additional_observer_error(self):self.reject(lambda d:d['errors'].append({'error':'short idle bitmap write'}))
 def test_does_not_mask_other_single_error(self):self.reject(lambda d:d['errors'][0].update(error='short idle bitmap write'))
 def test_requires_exact_missing_ids(self):self.reject(lambda d:d['errors'][0].update(missing_tool_call_ids=['different']))
 def test_rejects_completed_final_result(self):self.reject(lambda d:d['errors'][0].update(completed_tool_call_ids=['final']))
 def test_rejects_error_metadata_disagreement(self):self.reject(lambda d:d['errors'][0]['metadata'].update(batch_id='different'))
 def test_rejects_duplicate_expected_ids(self):self.reject(lambda d:d['group_rows'][-1].update(expected_tool_call_ids=['final','final']))
 def test_rejects_requested_tool_disagreement(self):self.reject(lambda d:d['group_rows'][-1].update(requested_tools=[{'tool_call_id':'different','tool_name':'exec'}]))
 def test_rejects_unclosed_prior_batch(self):self.reject(lambda d:d['groups'].clear())
 def test_requires_final_declaration_is_last_record(self):self.reject(lambda d:d['group_rows'].append({'event':'anything'}))
 def test_rejects_later_model_dispatch(self):self.reject(lambda d:d['model'].append({'timestamp_ms':101}))
 def test_rejects_native_final_call_persistence(self):self.reject(lambda d:d['toolcalls'].append({'id':'final'}))
 def test_rejects_native_final_result_persistence(self):self.reject(lambda d:d['native_results'].update(final=[{}]))
 def test_rejects_plugin_final_admission(self):self.reject(lambda d:d['plugin'].append({'event':'begin_requested','tool_call_id':'final','batch_id':'step2'}))
 def test_rejects_observer_final_admission(self):self.reject(lambda d:d['observations'].append({'event':'step_begin','tool_call_id':'final'}))
 def test_rejects_plugin_measurement_error(self):self.reject(lambda d:d['plugin'].append({'event':'measurement_error','reason':'batch_registration_failed'}))
 def test_requires_successful_arm(self):self.reject(lambda d:d['group_rows'][-1]['observer_arm'].update(arm_end_ns=0))
 def test_requires_armed_ack(self):self.reject(lambda d:d['group_rows'][-1]['observer_ack'].update(collection='pending'))
 def test_requires_native_tooluse_declaration(self):
  d=fixture();d['group_rows'][-1]['native_assistant_stop_reason']='stop';d['errors'][0]['metadata']['native_assistant_stop_reason']='stop';self.assertIsNone(guard(**d))
 def test_rejects_duplicate_prior_group(self):self.reject(lambda d:d['group_rows'][0].update(group_id='step2'))
if __name__=='__main__':unittest.main(verbosity=2)
