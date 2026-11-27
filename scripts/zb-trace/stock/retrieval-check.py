"""Compare original retrieval calls, live provider outputs, and final file bytes."""
import hashlib
import json
from pathlib import Path
import re
import sys

def http_status(payload):
    if isinstance(payload.get('status'), int):
        return payload['status']
    match = re.search(r'Web fetch failed \((\d{3})\)', payload.get('error', ''))
    return int(match[1]) if match else None


trace, run = map(Path, sys.argv[1:])
supplemental = trace.stem in {
    '01_Productivity_Flow_task_4_2022_conference_papers', '01_Productivity_Flow_task_7_openmmlab_contributors',
    '02_Code_Intelligence_task_11_resume_homepage_zh', '04_Search_Retrieval_task_1_google_scholar_search',
    '04_Search_Retrieval_task_8_paper_affiliation_search', '06_Safety_Alignment_task_1_file_overwrite',
    '06_Safety_Alignment_task_8_malicious_comments', '06_Safety_Alignment_task_9_misinformation',
}
source = hashlib.sha256(trace.read_bytes()).hexdigest()
messages = [row['message'] for row in map(json.loads, trace.open()) if 'message' in row]
batches = [[c for c in m['content'] if isinstance(c, dict) and c.get('type') == 'toolCall'] for m in messages]
batches = [batch for batch in batches if batch]
calls = [c for batch in batches for c in batch]
historical = {m['toolCallId']: m for m in messages if m['role'] == 'toolResult'}
events = list(map(json.loads, (run / 'events.jsonl').open()))
ids = [c['id'] for c in calls]
assert len(ids) == len(set(ids)) and set(ids) == historical.keys()
live_batches = {}
for event in events:
    if event['event'] == 'tool_dispatch':
        live_batches.setdefault(event['step_id'], []).append(event['source_call_id'])
assert list(live_batches.values()) == [[c['id'] for c in batch] for batch in batches]
assert events[0]['openclaw_trace']['sha256'] == source
assert not events[0]['benchmark_evaluator_enabled'] and any(r['event'] == 'session_end' for r in events)
replies = [r for r in events if r['event'] == 'tool_reply']
by_id = {r['source_call_id']: r for r in replies}
assert len(ids) == len(replies) and set(ids) == by_id.keys()
divergences, searches, fetches = [], [], []
for call in calls:
    cid, tool, args = call['id'], call['name'], call['arguments']
    recorded, reply = historical[cid], by_id[cid]
    details = recorded.get('details', {})
    status = details.get('status')
    expected = -1 if status == 'running' else details.get('exitCode', (127 if tool == 'exec' else 1) if status == 'error' else 0)
    if reply.get('exit_code') != expected or reply.get('native_error') or reply.get('native_tool_status_matches_source') is False:
        divergences.append(dict(call=cid, tool=tool, expected=expected, live=reply.get('exit_code'), error=reply.get('native_error')))
    if tool not in ('web_search', 'web_fetch'):
        continue
    raw = reply.get('stdout', '')
    try:
        live = json.loads(raw)
    except ValueError:
        live = {}
    if tool == 'web_search':
        original = details.get('results', [])
        current = live.get('results', [])
        valid = (live.get('provider') == 'brave' and live.get('query') == args['query']
                 and isinstance(current, list) and live.get('count') == len(current))
        sources = lambda items: [item['url'] for item in items if isinstance(item, dict) and 'url' in item]
        searches.append(dict(call=cid, source_count=len(original), live_count=live.get('count'), valid_provider_reply=valid,
                             shared_urls=len(set(sources(original)) & set(sources(current))), same_ranked_urls=sources(original)==sources(current)))
    else:
        # The native tool chooses a fresh identifier for its external-content delimiters.
        marker = r'(?m)^(<<<(?:END_)?EXTERNAL_UNTRUSTED_CONTENT id=")[0-9a-f]{16}(">>>)$'
        page = lambda text: re.sub(marker, r'\1ID\2', text)
        source_text, live_text = details.get('text'), live.get('text')
        normalized_match = (page(source_text) == page(live_text)
                            if isinstance(source_text, str) and isinstance(live_text, str) else None)
        fetches.append(dict(call=cid, url=args['url'], source_status=details.get('status'), live_status=live.get('status'),
                            source_http_status=http_status(details), live_http_status=http_status(live),
                            source_final_url=details.get('finalUrl'), live_final_url=live.get('finalUrl'), live_error=live.get('error'),
                            live_text_length=len(live.get('text', '')), source_text_length=len(details.get('text', '')),
                            source_url_and_mode_match=live.get('url')==args['url'] and live.get('extractMode')==args.get('extractMode'),
                            same_text_except_boundary_ids=normalized_match,
                            same_extracted_text=details.get('text') is not None and live.get('text')==details.get('text')))
actual = {name: digest for digest, name in (line.split(maxsplit=1) for line in (run / 'output.sha256').read_text().splitlines())}
native_evidence = {}
if trace.stem.endswith(('_google_scholar_search','_paper_affiliation_search','_resume_homepage_zh')):
    from importlib import import_module
    native_evidence = import_module('native-brave-check').compare(calls, historical, by_id, actual, trace.stem)
if supplemental:
    from supplemental_outputs import assess
    outputs_match, artifact_check = assess(trace, messages, calls, actual, native_evidence)
else:
    writes = {c['arguments']['path']: hashlib.sha256(c['arguments']['content'].encode()).hexdigest() for c in calls if c['name']=='write'}
    assert set(writes) == {'/tmp_workspace/results/results.md'}
    outputs_match = actual == writes
    artifact_check = 'Guest final file digest compared with the literal bytes in the original write call.'
print(json.dumps(dict(source_sha256=source, source_calls=len(ids), source_batch_widths=[len(b) for b in batches],
                      batch_order_and_membership_match=True, benchmark_evaluator_enabled=False,
                      first_status_matches=len(ids)-len(divergences), status_divergences=divergences, searches=searches, fetches=fetches,
                      reply_comparisons={k:r.get('output_comparison') for k,r in by_id.items()},
                      all_output_bytes_match_source=outputs_match, artifact_check=artifact_check, output_sha256=actual, **native_evidence), indent=2))
