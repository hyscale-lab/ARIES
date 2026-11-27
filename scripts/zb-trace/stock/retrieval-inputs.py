"""Stage a pinned public Harbor environment and known initial file contents for retrieval."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import tarfile
from urllib.request import urlopen

PIN = 'c880033a1bd4023ffe0175bb2983b46719a7e36b'
HARBOR = 'https://huggingface.co/datasets/InternLM/WildClawBench-Harbor/resolve/' + PIN
FILES = ('.wildclaw/run-warmup.sh', '.wildclaw/skills/agent-browser/SKILL.md', '.wildclaw/skills/agent-browser/_meta.json')
ROOT_READS = {'/root/SOUL.md', '/root/USER.md'}
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('trace', type=Path)
parser.add_argument('expected_digest')
parser.add_argument('output', type=Path)
a = parser.parse_args()
assert hashlib.sha256(a.trace.read_bytes()).hexdigest() == a.expected_digest
messages = [r['message'] for r in map(json.loads, a.trace.open()) if 'message' in r]
calls = [c for m in messages for c in m['content'] if isinstance(c, dict) and c.get('type') == 'toolCall']
results = {m['toolCallId']: m for m in messages if m['role'] == 'toolResult'}
name = a.trace.stem
tomllib = name == '04_Search_Retrieval_task_10_tomllib_trace'
inputs = {}
origins = {}
if tomllib:
    for c in calls:
        path = c['arguments'].get('path')
        if c['name'] == 'read' and path in ROOT_READS:
            inputs[path.lstrip('/')] = results[c['id']]['content'][0]['text'].encode()
            origins[path.lstrip('/')] = 'pinned initial read ' + c['id']
    assert set('/' + p for p in inputs) == ROOT_READS
else:
    for member in FILES:
        url = f'{HARBOR}/{name}/environment/{member}'
        data = urlopen(url, timeout=30).read()
        inputs['tmp_workspace/' + member] = data
        origins['tmp_workspace/' + member] = url
        if member.startswith('.wildclaw/skills/'):
            root = member.replace('.wildclaw/', 'root/', 1)
            inputs[root] = data
            origins[root] = url
    if '11_fuzzy_repo_search' not in name:
        marker = f'{HARBOR}/{name}/environment/.gitkeep'
        inputs['tmp_workspace/.gitkeep'] = urlopen(marker, timeout=30).read()
        assert inputs['tmp_workspace/.gitkeep'] == b''
        origins['tmp_workspace/.gitkeep'] = marker
fetches = [c['arguments'] for c in calls if c['name'] == 'web_fetch']
assert fetches and all(c['url'].startswith('https://') for c in fetches)
inputs['tmp/zb-approved-fetches.json'] = json.dumps(fetches, indent=2).encode()
origins['tmp/zb-approved-fetches.json'] = 'full original web_fetch arguments'
a.output.mkdir(exist_ok=True, parents=True)
archive = a.output / 'inputs.tar'
with tarfile.open(archive, 'w') as tar:
    for path, data in sorted(inputs.items()):
        info = tarfile.TarInfo(path)
        info.mode = 0o755 if path.endswith('.sh') else 0o644
        info.size = len(data)
        tar.addfile(info, io.BytesIO(data))
(a.output / 'SHA256SUMS').write_text(hashlib.sha256(archive.read_bytes()).hexdigest() + '  inputs.tar\n')
print(json.dumps(dict(source_sha256=a.expected_digest, harbor_revision=PIN, fetches=len(fetches),
                      warmup='absent' if tomllib else FILES[0], files=[
                          dict(path=p, sha256=hashlib.sha256(v).hexdigest(), origin=origins[p]) for p,v in sorted(inputs.items())]), indent=2))
