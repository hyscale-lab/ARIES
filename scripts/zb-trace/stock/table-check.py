"""Join the nineteen extracted tables to independently identified paper source bytes."""
from pathlib import Path
from hashlib import sha256
import sys,json

trace,run,fixture=map(Path,sys.argv[1:])
ref=json.loads(fixture.read_bytes())
source=sha256(trace.read_bytes()).hexdigest()
assert source==ref['source_trace_sha256']=='aa69da99ce7d25f0e9e591183a395ad1538c121e5a7d23cf3b8d1bc781c13502'
msgs=[l['message'] for l in map(json.loads,trace.open()) if 'message' in l]
calls=[c['id'] for m in msgs for c in m.get('content',[]) if c.get('type')=='toolCall']
original={m['toolCallId']:m for m in msgs if m['role']=='toolResult'}
ev=list(map(json.loads,(run/'events.jsonl').open()))
assert not ev[0]['benchmark_evaluator_enabled'] and any(e['event']=='session_end' for e in ev)
assert calls==[e['source_call_id'] for e in sorted((e for e in ev if e['event']=='tool_dispatch'),key=lambda e:e['sequence'])]
live={e['source_call_id']:e for e in ev if e['event']=='tool_reply'}
assert len(calls)==5 and set(calls)==original.keys()==live.keys()
diff=[dict(call=k,expected=m['details']['exitCode'],observed=live[k].get('exit_code')) for k,m in original.items() if live[k].get('exit_code')!=m['details']['exitCode']]
actual={Path(path).name:h for h,path in (l.split(maxsplit=1) for l in (run/'output.sha256').read_text().splitlines())}
print(json.dumps(dict(source_sha256=source,source_calls=len(calls),first_status_matches=len(calls)-len(diff),
    status_divergences=diff,all_output_bytes_match_source=actual==ref['output_sha256'],
    output_sha256=actual,source_archive_sha256=ref['source_sha256'],reply_comparisons={k:r.get('output_comparison') for k,r in live.items()}),indent=2))
