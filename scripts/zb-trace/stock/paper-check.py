"""Compare the native image error, first replies and independently rendered PNGs."""
import hashlib
import json
import sys
from pathlib import Path

trace, run = map(Path, sys.argv[1:])
digest = hashlib.sha256(trace.read_bytes()).hexdigest()
assert digest == '687aa2a8b04cdb4937c23afca77428cd6a194bb141af772453d988a06794e136'
calls = [c for row in map(json.loads,trace.open()) for c in row.get('message',{}).get('content',[]) if c.get('type')=='toolCall']
events = list(map(json.loads,(run/'events.jsonl').open()))
replies = {e['source_call_id']:e for e in events if e['event']=='tool_reply'}
assert len(calls)==len(replies)==16 and all(c['id'] in replies for c in calls)
assert any(e['event']=='session_end' for e in events) and not events[0]['benchmark_evaluator_enabled']
image, = [replies[c['id']] for c in calls if c['name']=='image']
diff = [dict(call=k,expected=r.get('expected_exit_code',0),observed=r.get('exit_code',0)) for k,r in replies.items() if r.get('expected_exit_code',0)!=r.get('exit_code',0)]
expected = {'/tmp_workspace/results/poster.png':'7176c26f4f37bc813248bf3388445193028258a55ec1e8c3f13fba294aa5d661',
            '/tmp_workspace/results/poster_preview.png':'6edb888a57dc56b1bf9c9346b39637f023f0cbddcd6883d0e6af6f0dee60c40b'}
hashes = {path:digest for digest,path in (line.split(maxsplit=1) for line in (run/'output.sha256').read_text().splitlines())}
match=image['output_comparison']=='matched_ignoring_trailing_newlines'
print(json.dumps(dict(source_sha256=digest,source_calls=len(calls),first_status_matches=len(calls)-len(diff),status_divergences=diff,
     native_image_reply_matches=match,all_output_bytes_match_source=match and hashes==expected,output_sha256=hashes,expected_render_sha256=expected),indent=2))
