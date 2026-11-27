"""Cross-check native process and image replies and the artifacts named by these three sources."""
import base64
from hashlib import sha256
from pathlib import PurePosixPath
import re


def process_details(details):
    fields = ('sessionId', 'name', 'status', 'exitCode', 'exitSignal')
    return {k: details[k] for k in fields if k in details}


def compare(calls, historical, live, actual, name):
    native = []
    for c in calls:
        r = live[c['id']]
        if 'native_tool_status_matches_source' not in r:
            continue
        recorded = historical[c['id']]
        source = recorded.get('details', {})
        observed = r.get('native_tool_details', {})
        assert source == r.get('source_native_tool_details', {})
        item = dict(call=c['id'], tool=c['name'], source=process_details(source), live=process_details(observed),
                    source_signal=r.get('source_native_tool_signal'), live_signal=r.get('native_tool_signal'),
                    status_matches=r['native_tool_status_matches_source'], output_comparison=r['output_comparison'])
        if c['name'] == 'process' and c['arguments']['action'] == 'list':
            item.update(source_processes=[process_details(p) for p in source['sessions']],
                        live_processes=[process_details(p) for p in observed.get('sessions', [])])
        if c['name'] == 'read':
            caption, attachment = recorded['content']
            observed_caption, _, data = r.get('stdout','').rpartition('\n')
            ref = base64.b64decode(attachment['data'], validate=True)
            got = base64.b64decode(data, validate=True) if observed_caption.startswith('Read image file [image/jpeg]') else None
            item.update(source_caption=caption['text'], live_caption=observed_caption if got else None,
                        source_jpeg_sha256=sha256(ref).hexdigest(), live_jpeg_sha256=sha256(got).hexdigest() if got else None,
                        attachment_matches=ref == got)
        native.append(item)
    assert native
    evidence = dict(native_tools=native)
    if name.endswith('_paper_affiliation_search'):
        expected = historical[calls[-1]['id']]['content'][0]['text'].splitlines()
        assert len(expected) == 82 and expected[0] == 'wrote 105 lines' and expected[-1] == '104 /tmp_workspace/results/results.md'
        observed = live[calls[-1]['id']].get('stdout', '').splitlines()
        observed_count = re.fullmatch(r'(\d+) /tmp_workspace/results/results\.md', observed[-1]) if observed else None
        printed_file = bool(observed_count and re.fullmatch(r'wrote \d+ lines', observed[0]))
        evidence['report_excerpt'] = dict(prefix_lines=80, prefix_matches=observed[1:81] == expected[1:81] if printed_file else None,
                                          newline_count_matches=int(observed_count[1]) == 104 if observed_count else None)
    if name.endswith('_resume_homepage_zh'):
        root = PurePosixPath('/tmp_workspace/results')
        contents = {}
        for c in calls:
            for path, delimiter, body in re.findall(r"(?m)^cat > (\S+) <<'(\w+)'\n([\s\S]*?)\n\2$", c['arguments'].get('command','')):
                contents[str(root/path)] = sha256((body+'\n').encode()).hexdigest()
        assert len(contents) == 5
        last = historical[calls[-1]['id']]['content'][0]['text']
        files = [str(root.parent/path) for path in re.findall(r'(?m)^ create mode 100644 (results/.*)$',last)]
        assert len(files) == 10 and contents.keys() <= set(files)
        evidence['resume_artifacts'] = dict(expected_files=files, missing=sorted(set(files)-actual.keys()),
                                            extra=sorted(actual.keys()-set(files)), source_literal_sha256=contents,
                                            literal_bytes_match={p: actual.get(p)==h for p,h in contents.items()})
    return evidence
