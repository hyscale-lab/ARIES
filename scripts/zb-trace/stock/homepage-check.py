"""Inspect the live checkout and artifacts, then compare them with original reply evidence."""
import hashlib
import json
import struct
import subprocess
import sys
from html.parser import HTMLParser
from pathlib import Path

SOURCE = 'f4d335cb20dbe8614dc7cc268634128d26eba95bb4719dbf32b229d5e12d7ffd'
REFERENCE = {
    'README.md': 'f86d93987acb476a2844d87a44eb5263d5fa535009aebf74077b6d286c9629c1',
    'assets/LOGO.svg': '1d2faf2b8f43b6de786f10868d7cacde5bbc2aebde136d37788f910241bc07d7',
    'assets/apple.jpg': 'cdedbfa6896ecdeff000235019adf36199caeafa6eafc8474dea4824e143599b',
}
REFERENCE_PNG = '2f97b936905ca5171e76c12b9a389421f2b60d160bbda056434a18d2d39e90e0'
REFERENCE_HTML = '7e6cfd6539f7f242c1062d5ac91b38eaa2984ab6a6802266318671b1689fc7d0'


class Images(HTMLParser):
    def __init__(self):
        super().__init__()
        self.sources = []

    def handle_starttag(self, tag, attrs):
        if tag == 'img':
            self.sources.append(dict(attrs).get('src', ''))


def inspect():
    root = Path('/tmp_workspace')
    repo = root / 'src/VLMEvalKit'
    commit = subprocess.run(['git', '-C', str(repo), 'rev-parse', 'HEAD'], capture_output=True, text=True)
    files = {}
    for filename in REFERENCE:
        path = repo / filename
        if path.is_file():
            files[filename] = hashlib.sha256(path.read_bytes()).hexdigest()
    info = dict(checkout_head=commit.stdout.strip(), checkout_files=files, outputs={})
    for filename in ('index.html', 'screenshot.png'):
        path = root / 'results' / filename
        if not path.is_file():
            continue
        data = path.read_bytes()
        info['outputs'][filename] = dict(sha256=hashlib.sha256(data).hexdigest(), bytes=len(data))
        if filename.endswith('.html'):
            parser = Images()
            parser.feed(data.decode())
            info['images'] = len(parser.sources)
            info['inline_images'] = sum(s.startswith('data:image/') for s in parser.sources)
        elif data[:8] == b'\x89PNG\r\n\x1a\n' and data[12:16] == b'IHDR':
            info['png_width_height'] = list(struct.unpack('>II', data[16:24]))
            info['png_ihdr_format'] = list(data[24:29])
    return info


def check(trace, run):
    assert hashlib.sha256(trace.read_bytes()).hexdigest() == SOURCE
    messages = [row['message'] for row in map(json.loads, trace.open()) if 'message' in row]
    calls = [c['id'] for m in messages for c in m.get('content', []) if c.get('type') == 'toolCall']
    original = {m['toolCallId']: m for m in messages if m['role'] == 'toolResult'}
    events = list(map(json.loads, (run / 'events.jsonl').open()))
    replies = [e for e in events if e['event'] == 'tool_reply']
    assert calls == [e['source_call_id'] for e in replies] == [e['source_call_id'] for e in events if e['event'] == 'tool_dispatch']
    assert len(calls) == len(original) == 8 and any(e['event'] == 'session_end' for e in events)
    assert not events[0]['benchmark_evaluator_enabled']
    expected = [0, 0, 127, 0, 0, -1, 0, 0]
    assert [r.get('expected_exit_code', 0 if r['operation'] == 'ReadFile' else None) for r in replies] == expected
    diff = [dict(call=c, expected=e, observed=r.get('exit_code')) for c, e, r in zip(calls, expected, replies) if e != r.get('exit_code')]
    files = json.loads((run / 'homepage-files.json').read_text())
    hashes = {Path(p).name: h for h, p in (l.split(maxsplit=1) for l in (run / 'output.sha256').read_text().splitlines())}
    assert hashes == {k: v['sha256'] for k, v in files['outputs'].items()}
    source_poll = original[calls[6]]['content'][0]['text']
    observed_poll = replies[6].get('stdout', '')
    status_lines = [line for line in source_poll.splitlines() if line.startswith(('title:', 'size:', 'images:', '/tmp_workspace/results/screenshot.png:'))]
    assert len(status_lines) == 4
    image_match = replies[-1]['output_comparison'] == 'matched_ignoring_trailing_newlines'
    html = files['outputs'].get('index.html', {})
    png = files['outputs'].get('screenshot.png', {})
    expected_dimensions = files.get('png_width_height') == [1440,5057]
    # GNU ls -lh rounds file sizes upward to the printed integer KiB.
    expected_png_size = (png.get('bytes', 0) + 1023) // 1024 == 891
    inline_images_match = files.get('images') == files.get('inline_images') == 8
    if 'png_ihdr_format' in files:
        expected_png_format = files['png_ihdr_format'] == [8, 2, 0, 0, 0]
    else:
        png_line = next((line for line in observed_poll.splitlines() if line.startswith('/tmp_workspace/results/screenshot.png:')), None)
        expected_png_format = png_line == status_lines[-1] if png_line else None
    artifact_bytes_disproved = not (html.get('bytes') == 265786 and expected_dimensions and expected_png_size
                                    and expected_png_format is not False and inline_images_match)
    artifact_check = ('Available original size, embedded-image and PNG format observations agree. '
                      if not artifact_bytes_disproved else 'An original output is missing or differs in size, embedded image count, PNG dimensions or format. ')
    artifact_check += 'The published PNG has no hash or pixels. Hashes from the independent publisher-image reconstruction are compared separately.'
    return dict(source_sha256=SOURCE, source_calls=8, first_status_matches=8-len(diff), status_divergences=diff,
                source_handle_matches=all(r.get('source_handle') == 'faint-crest' and r.get('source_process_session_id') == 280 for r in replies[5:7]),
                source_checkout_candidate='7055d3010c38ccb5dcae1bc9535ca19c7fe5d79f',
                source_checkout_basis='Latest public main commit preceding prompt; source omitted the commit ID.',
                compared_checkout_files={k: files['checkout_files'].get(k) == v for k, v in REFERENCE.items()},
                source_html_bytes=265786, reconstructed_html_sha256=REFERENCE_HTML,
                reconstructed_html_matches=hashes.get('index.html') == REFERENCE_HTML,
                reconstructed_png_sha256=REFERENCE_PNG, reconstructed_png_matches=hashes.get('screenshot.png') == REFERENCE_PNG,
                png_dimensions_match=expected_dimensions, original_rounded_png_size_matches=expected_png_size,
                all_eight_images_embedded=inline_images_match, original_png_format_matches=expected_png_format,
                original_render_checks={line: line in observed_poll for line in status_lines},
                native_image_reply_matches=image_match, output_sha256=hashes, all_output_bytes_match_source=False if artifact_bytes_disproved else None, artifact_check=artifact_check,
                original_png_comparison='Original published a rounded PNG size and dimensions, without a digest or pixels.',
                reply_comparisons={c: r.get('output_comparison') for c, r in zip(calls,replies)},
                observed=files)


if __name__ == '__main__':
    result = inspect() if sys.argv[1:] == ['inspect'] else check(*map(Path, sys.argv[1:]))
    print(json.dumps(result, indent=2))
