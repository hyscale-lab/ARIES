"""Check the pinned spreadsheet and arXiv shell replays with native Brave searches."""
import hashlib
import json
from pathlib import Path
import re
import sys

trace, run = map(Path, sys.argv[1:])
digest = hashlib.sha256(trace.read_bytes()).hexdigest()
excel = digest == 'd21b1a20750e0b72f6d8702ca1321bfdd391921886c2aa1850882bf060896432'
assert excel or digest == '209255b995e2554ee8bd061c723848844af0ac464054b5b62ab18bbbc3ac106b'
messages = [r['message'] for r in map(json.loads, trace.open()) if 'message' in r]
calls = [c for m in messages for c in m.get('content', []) if c.get('type') == 'toolCall']
historical = {m['toolCallId']: m for m in messages if m['role'] == 'toolResult'}
events = list(map(json.loads, (run / 'events.jsonl').open()))
ids = [c['id'] for c in calls]
assert len(ids) == (14 if excel else 27) and [r['source_call_id'] for r in events if r['event'] == 'tool_dispatch'] == ids
assert not events[0]['benchmark_evaluator_enabled'] and any(r['event'] == 'session_end' for r in events)
replies = [r for r in events if r['event'] == 'tool_reply']
assert len(replies) == len(ids) and {r['source_call_id'] for r in replies} == set(ids)
by_id = {r['source_call_id']: r for r in replies}
divergences = []
searches = []
for call in calls:
    r = by_id[call['id']]
    source = historical[call['id']]
    details = source.get('details', {})
    error_code = 1 if call['name'] == 'read' else 127
    expected = details.get('exitCode', error_code if details.get('status') == 'error' else 0)
    if r.get('exit_code') != expected or r.get('native_error'):
        divergences.append(dict(call=call['id'], expected=expected, observed=r.get('exit_code'), error=r.get('native_error')))
    if call['name'] == 'web_search':
        try:
            live = json.loads(r.get('stdout', ''))
        except ValueError:
            live = {}
        valid = (live.get('provider') == 'brave' and live.get('query') == call['arguments']['query']
                 and isinstance(live.get('results'), list) and live.get('count') == len(live['results']))
        searches.append(dict(call=call['id'], valid_provider_reply=valid, live_count=live.get('count'),
                             historical_count=json.loads(source['content'][0]['text'])['count']))
assert len(searches) == 3
hashes = {path: sha for sha, path in (line.split(maxsplit=1) for line in (run / 'output.sha256').read_text().splitlines())}
result = dict(source_sha256=digest, source_calls=len(calls), first_status_matches=len(calls)-len(divergences),
              status_divergences=divergences, searches=searches, output_sha256=hashes)
if excel:
    expected_hash = 'c392a2c101f371a01e6c8be27f4c9970549e9de7fa2bb719f8aae763527e2196'
    result['all_output_bytes_match_source'] = hashes == {'/tmp_workspace/results/results.md': expected_hash}
else:
    document = (run / 'arxiv_digest.md').read_text()
    expected = dict(re.findall(r'^\s*\d+ (\d{4}\.\d+) (.+)$', historical[ids[14]]['content'][0]['text'], re.M))
    observed = re.findall(r'^- \*\*(.+)\*\* \((\d{4}\.\d+)\)$', document, re.M)
    assert len(expected) == 130
    titles = {paper: title for title, paper in observed}
    differences = {paper: dict(historical=expected.get(paper), live=titles.get(paper))
                   for paper in sorted(titles.keys() | expected.keys()) if titles.get(paper) != expected.get(paper)}
    titles_match = len(observed) == 130 and not differences
    preview = historical[ids[22]]['content'][0]['text'].split('# ArXiv Daily Digest', 1)[1]
    preview_match = document.startswith('# ArXiv Daily Digest' + preview.rstrip('\n'))
    git_error = "is outside repository at '/root'"
    assert git_error in historical[ids[-1]]['content'][0]['text']
    final_git_check = git_error in (by_id[ids[-1]].get('stdout', '') + by_id[ids[-1]].get('stderr', ''))
    result.update(classified_titles_match=titles_match, classified_title_differences=differences,
                  historical_preview_matches=preview_match, final_git_rejected_outside_path=final_git_check,
                  all_output_bytes_match_source=None if titles_match and preview_match else False)
print(json.dumps(result, indent=2))
