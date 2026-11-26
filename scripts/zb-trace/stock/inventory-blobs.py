#!/usr/bin/env python3
"""Find local containerd content matching a reviewed image manifest and blob sizes."""
import argparse
import json
from pathlib import Path
import re

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('digests', nargs='+')
args = parser.parse_args()
if not all(re.fullmatch(r'[0-9a-f]{64}', x) for x in args.digests):
    parser.error('expected SHA-256 manifest digests')
roots = {Path('/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256'),
         Path('/var/lib/docker/containerd/daemon/io.containerd.content.v1.content/blobs/sha256')}
for pattern in ('*/runtime/root', '*/*/runtime/root', '*/*/*/runtime/root'):
    roots.update(p/'io.containerd.content.v1.content/blobs/sha256' for p in Path('/local').glob(pattern))
for root in sorted(roots):
    if not root.is_dir():
        continue
    for sha in args.digests:
        manifest = root/sha
        if not manifest.is_file():
            continue
        data = json.loads(manifest.read_bytes())
        resolved = sha
        if 'manifests' in data:
            arch = [m for m in data['manifests'] if m.get('platform', {}).get('architecture') == 'amd64'
                    and m['platform'].get('os') == 'linux']
            if len(arch) != 1 or not re.fullmatch(r'sha256:[0-9a-f]{64}', arch[0]['digest']):
                continue
            resolved = arch[0]['digest'][7:]
            if not (root/resolved).is_file():
                continue
            data = json.loads((root/resolved).read_bytes())
        if 'layers' not in data or 'config' not in data:
            continue
        entries = [data['config'], *data['layers']]
        if all(re.fullmatch(r'sha256:[0-9a-f]{64}', b['digest']) and
               (root/b['digest'][7:]).is_file() and (root/b['digest'][7:]).stat().st_size == b['size']
               for b in entries):
            print(json.dumps(dict(manifest=sha, platform_manifest=resolved, store=str(root), blobs=len(entries), bytes=sum(b['size'] for b in entries))))
