"""Build a Roy-only replay plan from the reviewed Harbor timeline and ATIF."""
import json
import sys
from pathlib import Path

source, trajectory = (json.loads(Path(p).read_text()) for p in sys.argv[1:3])
assert source['trial_id'] == 'd16a13fe-8381-4594-b6f6-60bb6f3aa660'
calls = {c['tool_call_id']: c['arguments']['cmd']
         for step in trajectory['steps'] for c in step.get('tool_calls', [])
         if c['function_name'] == 'exec_command'}
events = source['events']
ids = [e['call_id'] for e in events if e['kind'] == 'exec_command']
assert len(ids) == 5 and len(set(ids)) == 5 and set(ids) == set(calls)
assert all(e['kind'] != 'write_stdin' for e in events)
json.dump({'events': events, 'calls': calls}, sys.stdout)
