#!/usr/bin/env python3
"""Import the reviewed linux/amd64 manifest from a pinned containerd OCI export."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile


def import_image(archive, fixture, regctl, registry):
    metadata = json.loads((fixture / 'metadata.json').read_text())
    digest = metadata['image_digest']
    with tempfile.TemporaryDirectory(dir=archive.parent) as temporary, tarfile.open(archive) as tar:
        layout = Path(temporary)
        blobs = layout / 'blobs' / 'sha256'
        blobs.mkdir(parents=True)

        def copy_blob(descriptor):
            name = descriptor['digest']
            if not re.fullmatch(r'sha256:[0-9a-f]{64}', name):
                raise ValueError(f'unexpected OCI digest {name}')
            path = blobs / name[7:]
            if path.exists():
                return path
            member = tar.getmember('blobs/sha256/' + name[7:])
            if not member.isfile() or member.size != descriptor['size']:
                raise ValueError(f'incorrect blob type/size for {name}')
            with tar.extractfile(member) as source, path.open('xb') as out:
                shutil.copyfileobj(source, out, length=1 << 20)
            with path.open('rb') as source:
                if hashlib.file_digest(source, 'sha256').hexdigest() != name[7:]:
                    raise ValueError(f'corrupt blob for {name}')
            return path

        pending = json.load(tar.extractfile('index.json'))['manifests']
        while pending:
            desc = pending.pop()
            if desc['digest'] == digest:
                break
            if desc['mediaType'].endswith(('image.index.v1+json', 'manifest.list.v2+json')):
                pending.extend(json.loads(copy_blob(desc).read_text())['manifests'])
        else:
            raise ValueError(f'missing selected manifest {digest}')
        manifest = json.loads(copy_blob(desc).read_text())
        config = json.loads(copy_blob(manifest['config']).read_text())
        if (config['os'],config['architecture']) != ('linux','amd64'):
            raise ValueError('reviewed image requires linux/amd64')
        for layer in manifest['layers']:
            copy_blob(layer)
        desc['annotations'] = {'org.opencontainers.image.ref.name':'reviewed'}
        (layout / 'oci-layout').write_text('{"imageLayoutVersion":"1.0.0"}')
        (layout / 'index.json').write_text(json.dumps({'schemaVersion':2,'manifests':[desc]}))
        target = f"{registry}/library/{metadata['task']}:reviewed"
        subprocess.run([regctl,'image','copy',f'ocidir://{layout}:reviewed',target],check=True)
        resolved = subprocess.check_output([regctl,'image','digest',target],text=True).strip()
        if resolved != digest:
            raise ValueError(f'registry digest {resolved} differs from {digest}')
        env = config['config']['Env']
        if not any(entry.startswith('HOME=') for entry in env):
            env.append('HOME=/root')
        (fixture / 'image-env.json').write_text(json.dumps(env)+'\n')
        print(f"{target} {resolved}")


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archive',type=Path)
    parser.add_argument('fixture',type=Path)
    parser.add_argument('regctl')
    parser.add_argument('registry')
    import_image(**vars(parser.parse_args()))
