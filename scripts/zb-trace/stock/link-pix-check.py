"""Verify the two files against the original printed grid and description."""
import ast
import hashlib
import json
import re
import sys
from pathlib import Path

def digest(data):
    return hashlib.sha256(data).hexdigest()

def verify(trace,run):
    assert digest(trace.read_bytes())=='0f37ca65135cf0513ada9eef9fe1cc62dc8751cdc0f7fa872ad06d6e68e27709'
    messages=[r['message'] for r in map(json.loads,trace.open()) if 'message' in r]
    source_calls=[c for m in messages for c in m.get('content',[]) if isinstance(c,dict) and c.get('type')=='toolCall']
    logs=list(map(json.loads,(run/'events.jsonl').open()))
    calls=[r for r in logs if r['event']=='tool_dispatch']
    replies={r['source_call_id']:r for r in logs if r['event']=='tool_reply'}
    assert [c['source_call_id'] for c in calls]==[c['id'] for c in source_calls] and len(replies)==7
    assert not logs[0]['benchmark_evaluator_enabled'] and any(r['event']=='session_end' for r in logs)
    ordered=[replies[c['id']] for c in source_calls]
    assert [r['exit_code'] for r in ordered]==[0,0,1,127,1,0,0]
    assert all(r['output_comparison']=='matched_ignoring_trailing_newlines' for r in ordered[:6])
    source_replies=[m for m in messages if m.get('role')=='toolResult']
    grid=source_replies[-2]['details']['aggregated'].strip().splitlines()[-10:]
    palette={'B':(50,140,230),'R':(210,40,40),'W':(245,245,245),'T':(240,210,160),'G':(40,160,50)}
    assert len(grid)==10 and all(len(r)==10 and set(r)<=palette.keys() for r in grid)
    rgb=b''.join(b''.join(bytes(palette[c])*60 for c in row)*60 for row in grid)
    supplied=(run/'pixel-sha256').read_text().strip()
    assert supplied==digest(rgb)
    command=source_calls[-1]['arguments']['command']
    match=re.search(r"open\('/tmp_workspace/results/description.txt'.*?\.write\((.+?)\)\s*$",command,re.M)
    expected=digest(ast.literal_eval(match[1]).encode())
    outputs={Path(f).name:h for h,f in (s.split(maxsplit=1) for s in (run/'output.sha256').read_text().splitlines())}
    assert outputs.keys()=={'description.txt','result.png'} and outputs['description.txt']==expected
    return dict(source_calls=7,first_status_matches=7,raw_replies=[r['output_comparison'] for r in ordered],
                grid_matches=True,pixel_sha256=supplied,description_sha256=expected,output_sha256=outputs)
if __name__=='__main__':print(json.dumps(verify(Path(sys.argv[1]),Path(sys.argv[2])),indent=2))
