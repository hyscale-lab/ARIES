"""Check source names and intermediate text on a live public-web replay."""
import hashlib
import json
import re
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
def lengths(text):
    return {name:int(size) for name,size in re.findall(r"^(\S+\.md) (\d+) \[",text,re.MULTILINE)}
expected_lengths=lengths(source[calls[-2]]['content'][0]['text'])
observed_lengths=lengths(replies[calls[-2]].get('stdout',''))
assert expected_lengths.keys()==set(names)
# Only the final call edits this one file after the per-file length check.
unchanged=set(names)-{'刘协.md'}
differences={n:dict(source=expected_lengths[n],live=observed_lengths[n]) for n in sorted(unchanged & observed_lengths.keys())
             if expected_lengths[n]!=observed_lengths[n]}
disproved=set(names)!=set(hashes) or bool(differences)
print(json.dumps(dict(source_sha256=digest,source_calls=len(calls),first_status_matches=len(calls)-len(diff),status_divergences=diff,
    names_match=sorted(names)==sorted(hashes),source_file_count=len(names),output_sha256=hashes,known_final_length_differences=differences,all_output_bytes_match_source=False if disproved else None,
    artifact_check='Historical reply lists nine names and text lengths. Eight files remained unedited after the character-count check. It supplies no full-file digest.',
    reply_comparisons={k:r.get('output_comparison') for k,r in replies.items()}),indent=2))
