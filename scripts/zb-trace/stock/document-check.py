"""Compare every document output and source handle with the original trace and fixture."""
import ast
import hashlib
import json
import sys
import tarfile
from pathlib import Path


def sha(data):
    return hashlib.sha256(data).hexdigest()


def pdf_outputs(calls, fixture):
    script = next(c['arguments']['command'] for c in calls if 'cat > /tmp_workspace/build_results.py' in c['arguments'].get('command', ''))
    body = ast.parse(script.split('\n', 1)[1].split('\nPY\n', 1)[0]).body
    values = {n.targets[0].id: ast.literal_eval(n.value) for n in body if isinstance(n, ast.Assign) and isinstance(n.targets[0], ast.Name) and n.targets[0].id in ('titles', 'cats', 'L')}
    titles, cats, lines = values['titles'], values['cats'], values['L']
    names = {k: v.translate(str.maketrans({' ': '_', '/': '_', '\\': '_'})) + '.pdf' for k, v in titles.items()}
    with tarfile.open(fixture) as outer:
        with tarfile.open(fileobj=outer.extractfile('./papers.tar'), mode='r|') as papers:
            outputs = {'papers/' + names[Path(m.name).name]: hashlib.file_digest(papers.extractfile(m), 'sha256').hexdigest() for m in papers if m.isfile()}
    assert len(outputs) == len(names) == 65
    lines.extend(f'| {k} | {names[k]} |' for k in sorted(names))
    lines.extend(['', '### Classification'])
    for cat, ids in cats.items():
        lines.extend([f'#### {cat}', *('- ' + titles[i] for i in ids), ''])
    lines.extend(next(ast.literal_eval(n.value) for n in body if isinstance(n, ast.AugAssign) and ast.literal_eval(n.value)[0] == '### Caption-Related Papers'))
    edit = next(c['arguments'] for c in calls if c['name'] == 'edit')
    markdown = '\n'.join(lines)
    assert markdown.count(edit['oldText']) == 1
    outputs['paper_digest.md'] = sha(markdown.replace(edit['oldText'], edit['newText']).encode())
    return outputs


def verify(trace, run, fixture):
    source = sha(trace.read_bytes())
    assert source in {'5d721acdbeeedc7cb51a7779d7a8328756422f2d512620bcb400a5f50663be1f', '7a020f97fe201c8153c08e219839b7a6b6d77ae6f6755a42588c27a2f42fef39'}
    messages = [r['message'] for r in map(json.loads, trace.open()) if 'message' in r]
    calls = [c for m in messages for c in m.get('content', []) if c.get('type') == 'toolCall']
    native = {m['toolCallId']: m for m in messages if m['role'] == 'toolResult'}
    logs = list(map(json.loads, (run / 'events.jsonl').open()))
    ids = [row['source_call_id'] for row in logs if row['event'] == 'tool_dispatch']
    assert ids == [c['id'] for c in calls]
    assert not logs[0]['benchmark_evaluator_enabled'] and any(r['event'] == 'session_end' for r in logs)
    replies = [r for r in logs if r['event'] == 'tool_reply']
    assert len(replies) == len(ids) and {r['source_call_id'] for r in replies} == set(ids)
    mismatches = []
    streams = {}
    for r in replies:
        m = native[r['source_call_id']]
        d = m.get('details', {})
        expected = d.get('exitCode', {'running': -1, 'error': 1 if m['toolName'] == 'read' else 127}.get(d.get('status'), 0))
        if r.get('exit_code') != expected:
            mismatches.append(dict(call=r['source_call_id'], expected=expected, observed=r.get('exit_code')))
        if d.get('sessionId'):
            handle = d['sessionId']
            assert r['source_handle'] == handle
            stream = r['guest_process_timing']['milestones']['command.started']['at']
            if d['status'] == 'running':
                assert handle not in streams
                streams[handle] = stream
            else:
                assert stream == streams[handle]
    actual = {file.removeprefix('/tmp_workspace/results/'): h for h, file in (line.split(maxsplit=1) for line in (run / 'output.sha256').read_text().splitlines())}
    if calls[-2]['name'] == 'write':
        expected = {'results.md': sha(calls[-2]['arguments']['content'].encode())}
    else:
        expected = pdf_outputs(calls, fixture)
    report = dict(source_sha256=source, source_calls=len(ids), first_status_matches=len(ids)-len(mismatches), status_divergences=mismatches,
                  benchmark_evaluator_enabled=False, source_handles_to_live_stream_start_utc=streams,
                  reply_comparisons={r['source_call_id']:r['output_comparison'] for r in replies},
                  all_output_bytes_match_source=actual == expected, output_sha256=actual)
    print(json.dumps(report, indent=2), flush=True)
    assert actual == expected, 'document bytes differ from source'


if __name__ == '__main__':
    verify(*(Path(s) for s in sys.argv[1:]))
