"""Verify native replies and the final synthetic filesystem against a pinned recording."""
import hashlib
import json
import sys
from pathlib import Path

trace, run = map(Path, sys.argv[1:])
digest = hashlib.sha256(trace.read_bytes()).hexdigest()
tasks = {
    'b81663dd33480607ca01403d075a5a2cfa10a1fce0945df9d0c17cb9ac597f25': 'schedule',
    '653b6bbe76655ec283f849e2d4f731f78e5b7adb78ea11ff4cbc833d297db0fe': 'trash',
    'dce6d103d9d7fb344b44a6e4b37bab46461e92f89e8960ca3af5907931f1c3bd': 'chapter',
}
task = tasks[digest]
messages = [row['message'] for row in map(json.loads,trace.open()) if 'message' in row]
calls = [c for m in messages for c in m.get('content',[]) if c.get('type') == 'toolCall']
source = {m['toolCallId']:m for m in messages if m['role']=='toolResult'}
events = list(map(json.loads,(run/'events.jsonl').open()))
assert not events[0]['benchmark_evaluator_enabled'] and any(e['event']=='session_end' for e in events)
dispatch = sorted((e for e in events if e['event']=='tool_dispatch'),key=lambda e:e['sequence'])
assert [e['source_call_id'] for e in dispatch] == [c['id'] for c in calls]
replies = {e['source_call_id']:e for e in events if e['event']=='tool_reply'}
assert set(replies)==set(source)=={c['id'] for c in calls}
divergences=[]
for cid,m in source.items():
    details=m.get('details',{})
    status=details.get('exitCode',1 if details.get('status')=='error' else 0)
    if replies[cid].get('exit_code')!=status:
        divergences.append(dict(call=cid,expected=status,observed=replies[cid].get('exit_code')))
actual, path=(run/'output.sha256').read_text().strip().split(maxsplit=1)
if task=='schedule':
    fixture=Path('/local/zb-study3-stock/local-sol/schedule-edit/environment.tar')
    import tarfile
    with tarfile.open(fixture) as t:
        member=next(m for m in t if m.name.removeprefix('./')=='scheduler.txt')
        before=t.extractfile(member).read().decode()
    edit=next(c['arguments'] for c in calls if c['name']=='edit')
    assert before.count(edit['oldText'])==1 and path==edit['path']
    after=before.replace(edit['oldText'],edit['newText'])
    reads=[source[c['id']]['content'][0]['text'] for c in calls if c['name']=='read' and c['arguments']['path']==path]
    assert [s.rstrip('\n') for s in reads]==[s.rstrip('\n') for s in (before,after)]
    data=after.encode()
elif task=='chapter':
    write=next(c['arguments'] for c in calls if c['name']=='write')
    assert path==write['path']
    data=write['content'].encode()
else:
    assert path=='-' and len(calls)==1
    data=b''
expected=hashlib.sha256(data).hexdigest()
print(json.dumps(dict(source_sha256=digest,source_calls=len(calls),first_status_matches=len(calls)-len(divergences),
    status_divergences=divergences,all_output_bytes_match_source=actual==expected,output_sha256={path:actual},
    reply_comparisons={k:r.get('output_comparison') for k,r in replies.items()}),indent=2))
assert actual==expected
