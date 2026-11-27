"""Pack the original publisher fixture plus trace-attested initial local read files."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import tarfile

parser = argparse.ArgumentParser()
parser.add_argument('slug')
parser.add_argument('traces', type=Path)
parser.add_argument('environment', type=Path)
parser.add_argument('target', type=Path)
a = parser.parse_args()
spec = json.loads(Path(__file__).with_name('supplemental-sources.json').read_text())[a.slug]
trace = a.traces / spec['trace']
assert hashlib.sha256(trace.read_bytes()).hexdigest() == spec['sha256']
messages = [r['message'] for line in trace.open() if 'message' in (r := json.loads(line))]
replies = {m['toolCallId']: m for m in messages if m['role']=='toolResult'}
root_reads = {'/root/SOUL.md', '/root/USER.md', '/root/BOOTSTRAP.md', '/root/skills/web-search/SKILL.md'}
files = {}
for p in a.environment.rglob('*'):
    if p.is_file() and p.name != '.gitkeep':
        files['tmp_workspace/'+str(p.relative_to(a.environment))] = p.read_bytes()
fetches = []
for m in messages:
    if m['role'] != 'assistant':continue
    for c in m['content']:
        if c.get('type') != 'toolCall':continue
        args = c['arguments']
        if c['name']=='web_fetch':fetches.append(args)
        if c['name']=='read' and (name:=args.get('path',args.get('file_path'))) in root_reads:
            reply=replies[c['id']]
            if reply.get('details',{}).get('status')!='error':
                data=reply['content'][0]['text'].encode();key=name.lstrip('/')
                assert files.setdefault(key,data)==data
if fetches:files['tmp/zb-approved-fetches.json']=json.dumps(fetches).encode()
a.target.mkdir(parents=True,exist_ok=True)
with tarfile.open(a.target/'inputs.tar','w') as bundle:
    for name,data in sorted(files.items()):
        info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o755 if name.endswith('.sh') else 0o644
        bundle.addfile(info,io.BytesIO(data))
(a.target/'SHA256SUMS').write_text(hashlib.sha256((a.target/'inputs.tar').read_bytes()).hexdigest()+'  inputs.tar\n')
