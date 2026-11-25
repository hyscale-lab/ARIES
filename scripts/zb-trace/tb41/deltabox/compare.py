"""Compare the same recorded wait and following first reply in two Roy pilots."""
import json
import statistics
import sys
from pathlib import Path

def rows(path):
    return [json.loads(line) for line in Path(path).read_text().splitlines()]

def one(items, predicate):
    result = [item for item in items if predicate(item)]
    assert len(result) == 1, len(result)
    return result[0]

delta, ae, samples = map(rows, sys.argv[1:4])
worker = sys.argv[4]
assert worker in ('amd182', 'amd191')
dg = [r for r in delta if r['event'] == 'guest']
guest = [r['payload'] for r in dg]
def devent(event):
    return one(guest, lambda r: r['event'] == event)
def duration(prefix):
    return (devent(prefix+'_end')['guest_monotonic_ns']-devent(prefix+'_start')['guest_monotonic_ns'])/1e6

def delta_wait(kind):
    return one(dg, lambda r: r['payload']['event'] == kind and r['payload'].get('line') == 37)['host_monotonic_ns']
def agent_wait(kind):
    return one(ae, lambda r: r['event'] == kind and r.get('rollout_line') == 37)['monotonic_ns']

ds, dt = delta_wait('wait_start'), delta_wait('wait_end')
as_, at = agent_wait('wait_start'), agent_wait('wait_end')
dpss = [r['pss_kib']/1024 for r in delta if r['event'] == 'pss' and ds <= r['host_monotonic_ns'] <= dt]
apss = [r['pss_bytes']/2**20 for r in samples if 'pss_bytes' in r and as_ <= r['begin_ns'] and r['end_ns'] <= at]
assert dpss and apss
replies = [r for r in guest if r['event'] == 'reply']
agent_replies = [r for r in ae if r['event'] == 'tool_reply']
assert len(replies) == len(agent_replies) == 5
assert [(r['call_id'], r['rc']) for r in replies] == [(r['source_call_id'], r['expected_exit_code']) for r in agent_replies]
assert all(r['exit_code'] == r['expected_exit_code'] for r in agent_replies)
def guest_call(event):
    return one(guest, lambda r: r['event'] == event and r.get('sequence') == 4)['guest_monotonic_ns']
def agent_call(event):
    return one(ae, lambda r: r['event'] == event and r.get('sequence') == 4)['monotonic_ns']
output = {'line': 37, 'wait_requested_s': 20.009,
          'delta': {'worker':'amd182','samples':len(dpss),'pss_mean_mib':statistics.mean(dpss),'pss_min_mib':min(dpss),'pss_max_mib':max(dpss), 'wait_observed_host_s':(dt-ds)/1e9,
                    'initial_checkpoint_ms':duration('initial_checkpoint'), 'initial_restore_ms':duration('initial_restore'), 'wait_checkpoint_ms':duration('checkpoint'), 'wait_restore_ms':duration('restore'), 'next_first_reply_ms':(guest_call('reply')-guest_call('dispatch'))/1e6},
          'agentenv': {'worker':worker,'samples':len(apss),'pss_mean_mib':statistics.mean(apss),'pss_min_mib':min(apss),'pss_max_mib':max(apss),'wait_observed_host_s':(at-as_)/1e9, 'next_first_reply_ms':(agent_call('tool_reply')-agent_call('tool_dispatch'))/1e6},
          'call_ids_and_exits':[(r['call_id'],r['rc']) for r in replies]}
json.dump(output,sys.stdout,indent=2); print()
