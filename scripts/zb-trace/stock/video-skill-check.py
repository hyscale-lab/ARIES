"""Compare the recorded writes and final edit with the live files and ZIP members."""
import hashlib
import json
from pathlib import Path
import sys
from zipfile import ZipFile

workspace = Path('/tmp_workspace/Auto-Research-Skills')
archive = workspace / 'dist/video-analysis.skill'


def inspect_archive():
    if not archive.is_file():
        return {}
    with ZipFile(archive) as bundle:
        return {p.filename:hashlib.sha256(bundle.read(p)).hexdigest()
                for p in bundle.infolist() if not p.is_dir()}


def check(trace, run):
    digest = hashlib.sha256(trace.read_bytes()).hexdigest()
    assert digest == '56ef19ba1c9b3096dab3522e9d45080fc5457dabef0082fbaeaae0d1e6fddc68'
    messages = [r.get('message', {}) for r in map(json.loads, trace.open())]
    calls = [c for m in messages if m.get('role') == 'assistant'
             for c in m['content'] if c.get('type') == 'toolCall']
    events = list(map(json.loads, (run / 'events.jsonl').open()))
    replies = {e['source_call_id']:e for e in events if e['event'] == 'tool_reply'}
    assert len(calls) == len(replies) == 24 and set(replies) == {c['id'] for c in calls}
    assert any(e['event'] == 'session_end' for e in events)
    files = {}
    for c in calls:
        a = c['arguments']
        if c['name'] == 'write':
            files[a['path']] = a['content']
        if c['name'] == 'edit':
            assert files[a['path']].count(a['oldText']) == 1
            files[a['path']] = files[a['path']].replace(a['oldText'], a['newText'])
    expected = {p:hashlib.sha256(body.encode()).hexdigest() for p,body in files.items()}
    assert len(expected) == 9
    live = {p:h for h,p in (l.split(maxsplit=1) for l in (run/'output.sha256').read_text().splitlines())}
    members = json.loads((run/'video-skill-members.json').read_text())
    packaged = {str(Path(p).relative_to(workspace/'skills')):h for p,h in expected.items()
                if Path(p).is_relative_to(workspace/'skills')}
    changed = [p for p,h in expected.items() if live.get(p) != h]
    diff = [dict(call=k,expected=e.get('expected_exit_code',0),observed=e.get('exit_code'))
            for k,e in replies.items() if e.get('exit_code') != e.get('expected_exit_code',0)]
    content_matches = not changed and members == packaged and str(archive) in live
    return dict(source_sha256=digest,source_calls=len(calls),first_status_matches=len(calls)-len(diff),
                status_divergences=diff,source_file_count=len(expected),divergent_files=changed,
                package_members_match=members == packaged,package_member_count=len(members),
                output_sha256=live,all_output_bytes_match_source=None if content_matches else False,
                artifact_check='Nine recorded file contents and eight packaged members. Original ZIP byte digest unavailable.')


if __name__ == '__main__':
    result = inspect_archive() if sys.argv[1:] == ['inspect'] else check(*map(Path,sys.argv[1:]))
    print(json.dumps(result,indent=2))
