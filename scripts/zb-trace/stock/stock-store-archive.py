#!/usr/bin/env python3
"""Export the manifest and its dependencies from an existing local OCI content store."""
import argparse
import io
import json
from pathlib import Path
import re
import tarfile

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('store',type=Path)
p.add_argument('digest')
p.add_argument('output',type=Path)
args=p.parse_args()
if not re.fullmatch(r'sha256:[0-9a-f]{64}',args.digest):
    p.error('expected a pinned manifest digest')
src=args.store/args.digest[7:]
manifest=json.loads(src.read_bytes())
desc=dict(mediaType=manifest['mediaType'],digest=args.digest,size=src.stat().st_size)
entries=[desc]
if 'manifests' in manifest:
    platform, = [m for m in manifest['manifests'] if m.get('platform',{}).get('os')=='linux'
                 and m['platform'].get('architecture')=='amd64']
    assert re.fullmatch(r'sha256:[0-9a-f]{64}',platform['digest'])
    manifest=json.loads((args.store/platform['digest'][7:]).read_bytes())
    entries.append(platform)
entries.extend([manifest['config'],*manifest['layers']])
with tarfile.open(args.output,'x') as tar:
    index=json.dumps(dict(schemaVersion=2,manifests=[desc])).encode()
    header=tarfile.TarInfo('index.json')
    header.size=len(index)
    tar.addfile(header,io.BytesIO(index))
    for item in entries:
        assert re.fullmatch(r'sha256:[0-9a-f]{64}',item['digest'])
        name=item['digest'][7:]
        path=args.store/name
        assert path.stat().st_size == item['size']
        tar.add(path,arcname='blobs/sha256/'+name,recursive=False)
