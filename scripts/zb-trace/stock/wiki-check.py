"""Check source names and intermediate text on a live public-web replay."""
import hashlib
import json
import sys
from pathlib import Path

trace, run = map(Path, sys.argv[1:])
digest = hashlib.sha256(trace.read_bytes()).hexdigest()
assert digest == '299e58cc16a532659b379102be2543c43f6dd24f397b94f460b04dc4d42dc25c'
ms = [r['message'] for r in map(json.loads,trace.open()) if 'message' in r]
calls = [c['id'] for m in ms for c in m.get('content',[]) if c.get('type')=='toolCall']
source = {m['toolCallId']:m for m in ms if m['role']=='toolResult'}
events = list(map(json.loads,(run/'events.jsonl').open()))
replies = {r['source_call_id']:r for r in events if r['event']=='tool_reply'}
assert len(calls)==len(replies)==14 and set(calls)==replies.keys()==source.keys()
assert any(e['event']=='session_end' for e in events) and not events[0]['benchmark_evaluator_enabled']
diff=[]
for k,r in replies.items():
    d=source[k].get('details',{})
    expected=d.get('exitCode',127 if d.get('status')=='error' else 0)
    if r.get('exit_code')!=expected:diff.append(dict(call=k,expected=expected,observed=r.get('exit_code')))
names=source[calls[-1]]['content'][0]['text'].strip().splitlines()
hashes={Path(path).name:h for h,path in (line.split(maxsplit=1) for line in (run/'output.sha256').read_text().splitlines())}
assert len(names)==9
print(json.dumps(dict(source_sha256=digest,source_calls=len(calls),first_status_matches=len(calls)-len(diff),status_divergences=diff,
    names_match=sorted(names)==sorted(hashes),source_file_count=len(names),output_sha256=hashes,all_output_bytes_match_source=None,
    artifact_check='Historical reply lists nine names and text lengths. It supplies no full-file digest.',
    reply_comparisons={k:r.get('output_comparison') for k,r in replies.items()}),indent=2))
