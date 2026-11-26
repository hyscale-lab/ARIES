"""Match observed SAM3 first replies and compare the output with the source prediction file."""
from pathlib import Path
import hashlib,json,sys

trace,run=map(Path,sys.argv[1:])
messages=[d['message'] for d in map(json.loads,trace.open()) if 'message' in d]
calls=[c for m in messages for c in m.get('content',[]) if c.get('type')=='toolCall']
source={m['toolCallId']:m for m in messages if m['role']=='toolResult'}
events=list(map(json.loads,(run/'events.jsonl').open()))
replies={r['source_call_id']:r for r in events if r['event']=='tool_reply'}
assert [e["source_call_id"] for e in sorted((e for e in events if e["event"]=="tool_dispatch"),key=lambda e:e["sequence"])]==[c["id"] for c in calls]
assert {c['id'] for c in calls}==source.keys()==replies.keys()
assert any(e['event']=='session_end' for e in events) and not events[0]['benchmark_evaluator_enabled']
diff=[]
for c in calls:
    r=replies[c['id']]
    if r.get('expected_exit_code',0)!=r.get('exit_code') or r.get('native_error'):
        diff.append(dict(call=c['id'],expected=r.get('expected_exit_code',0),observed=r.get('exit_code')))
digest=hashlib.sha256(trace.read_bytes()).hexdigest()
if digest=='6fdb46f630e5f42e9b7dd8225f8c59785e6bc9e8d8881b3b3744a57d8cfbaada':
    displayed,=[source[c['id']]['content'][0]['text'] for c in calls if c['arguments'].get('command')=='cat results/predictions.json']
    expected=hashlib.sha256((displayed+'\n').encode()).hexdigest()
    image,=[replies[c['id']] for c in calls if c['name']=='image']
    image_match=image['output_comparison']=='matched_ignoring_trailing_newlines'
else:
    assert digest=='f9c8fb5e9bb4b9dc90005a65e86952af6a7adbc78a616c81f1c1cfaead2a47db'
    expected='3b03bb2b1bdee2220614cc6672df21ec32bd63f60722d8dc0340de8d5cebba87'
    assert any(expected+'  results/predictions.json' in m['content'][0]['text'] for m in source.values())
    image_match=None
hashes={path:h for h,path in (s.split(maxsplit=1) for s in (run/'output.sha256').read_text().splitlines())}
print(json.dumps(dict(source_sha256=digest,source_calls=len(calls),first_status_matches=len(calls)-len(diff),status_divergences=diff,
    expected_predictions_sha256=expected,native_image_reply_matches=image_match,output_sha256=hashes,
    all_output_bytes_match_source=image_match is not False and hashes=={'/tmp_workspace/results/predictions.json':expected}),indent=2))
