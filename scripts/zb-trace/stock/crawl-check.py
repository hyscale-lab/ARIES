"""Verify the historical crawl's file inventory and every article exposed by source reads."""
import hashlib,json,re,sys
from pathlib import Path

trace,run=map(Path,sys.argv[1:])
digest=hashlib.sha256(trace.read_bytes()).hexdigest()
assert digest=='6811509f8ac7460ddcd9be97b3a68166384941e8795837a0b3365560a24cfb61'
ms=[x['message'] for x in map(json.loads,trace.open()) if 'message' in x]
calls=[c for m in ms for c in m.get('content',[]) if c.get('type')=='toolCall']
source={m['toolCallId']:m for m in ms if m['role']=='toolResult'}
events=list(map(json.loads,(run/'events.jsonl').open()))
live={e['source_call_id']:e for e in events if e['event']=='tool_reply'}
assert set(live)==set(source)=={c['id'] for c in calls} and len(live)==11
assert any(e['event']=='session_end' for e in events) and not events[0]['benchmark_evaluator_enabled']
diff=[dict(call=k,expected=r.get('expected_exit_code',0),observed=r.get('exit_code')) for k,r in live.items()
      if r.get('exit_code')!=r.get('expected_exit_code',0) or r.get('native_error')]
hashes={p:h for h,p in (l.split(maxsplit=1) for l in (run/'output.sha256').read_text().splitlines())}
final=source[calls[-1]['id']]['content'][0]['text']
assert final.startswith('dirs 50\ntext 50 jpg 43 jsonl 50\nbytes 6778603\n')
reads={c['arguments']['path']:live[c['id']]['output_comparison'] for c in calls if c['name']=='read'}
print(json.dumps(dict(source_sha256=digest,source_calls=len(calls),first_status_matches=len(calls)-len(diff),status_divergences=diff,
     native_read_checks=reads,source_inventory_matches=live[calls[-1]['id']]['output_comparison']=='matched_ignoring_trailing_newlines',
     source_jpegs=43,live_jpegs=sum(p.endswith('.jpg') for p in hashes),source_text_files=50,live_text_files=sum(p.endswith('text.md') for p in hashes),
     output_sha256=hashes,all_output_bytes_match_source=None,artifact_check='Historical trace includes counts, byte total and selected article and summary reads. Full corpus hashes are unavailable.'),indent=2))
