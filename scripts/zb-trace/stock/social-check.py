"""Check the local Slack simulator runs against the recorded draft and report."""
import ast
import hashlib
import json
from pathlib import Path
import sys

trace,run=map(Path,sys.argv[1:])
messages=[r['message'] for r in map(json.loads,trace.open()) if 'message' in r]
calls=[c for m in messages for c in m.get('content',[]) if c.get('type')=='toolCall']
original={m['toolCallId']:m for m in messages if m['role']=='toolResult'}
events=list(map(json.loads,(run/'events.jsonl').open()))
assert not events[0]['benchmark_evaluator_enabled'] and any(e['event']=='session_end' for e in events)
assert [d['source_call_id'] for d in sorted((d for d in events if d['event']=='tool_dispatch'),key=lambda d:d['sequence'])]==[c['id'] for c in calls]
live={r['source_call_id']:r for r in events if r['event']=='tool_reply'}
assert len(live)==len(calls) and live.keys()==original.keys()
diff=[]
for k,m in original.items():
    d=m.get('details',{})
    expected=d.get('exitCode',1 if d.get('status')=='error' and m['toolName']=='read' else 127 if d.get('status')=='error' else 0)
    if expected!=live[k].get('exit_code'):
        diff.append(dict(call=k,expected=expected,observed=live[k].get('exit_code')))
writes=[c['arguments']['content'] for c in calls if c['name']=='write' and c['arguments']['path']=='/tmp_workspace/results/results.md']
if writes:
    content,=writes
elif '_task_6_' in trace.stem:
    script,=[c['arguments']['command'] for c in calls if 'cat > /tmp/report.md' in c['arguments'].get('command','')]
    prefix,rest=script.split('\nEOF\n')
    code=ast.parse(rest.split('\n',1)[1].split('\nPY\n')[0])
    suffix,=[ast.literal_eval(n.args[0]) for n in ast.walk(code) if isinstance(n,ast.Call) and isinstance(n.func,ast.Attribute) and isinstance(n.func.value,ast.Name) and n.func.value.id=='f' and n.func.attr=='write']
    content=prefix.split('\n',1)[1]+'\n'+suffix
else:
    c,=[c for c in calls if 'save_report.py' in c['arguments'].get('command','')]
    code=ast.parse(c['arguments']['command'].split('\n',1)[1].split('\nPY\n')[0])
    assignments={n.targets[0].id:n.value for n in code.body if isinstance(n,ast.Assign) and isinstance(n.targets[0],ast.Name)}
    response,_=json.JSONDecoder().raw_decode(original[c['id']]['content'][0]['text'])
    replacements={'draft':ast.literal_eval(assignments['draft']),'json.dumps(resp, ensure_ascii=False)':json.dumps(response,ensure_ascii=False)}
    assert isinstance(assignments['report'],ast.JoinedStr)
    content=''.join(n.value if isinstance(n,ast.Constant) else replacements[ast.unparse(n.value)] for n in assignments['report'].values)
expected=hashlib.sha256(content.encode()).hexdigest()
actual,path=(run/'output.sha256').read_text().split()
assert path=='/tmp_workspace/results/results.md'
print(json.dumps(dict(source_sha256=hashlib.sha256(trace.read_bytes()).hexdigest(),source_calls=len(calls),
    first_status_matches=len(calls)-len(diff),status_divergences=diff,expected_output_sha256=expected,
    all_output_bytes_match_source=actual==expected,output_sha256={path:actual},
    reply_comparisons={k:r.get('output_comparison') for k,r in live.items()}),indent=2))
