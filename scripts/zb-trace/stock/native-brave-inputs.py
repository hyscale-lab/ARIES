"""Prepare the native manifest and source-era Playwright inputs after supplemental-inputs.py."""
from hashlib import sha256
from io import BytesIO
import json
from pathlib import Path
import subprocess
import sys
import tarfile
from urllib.request import urlopen

trace, stage = map(Path, sys.argv[1:])
stock = Path(__file__).resolve().parent
manifest = subprocess.check_output(['node', stock/'../../../cmd/aries-agentenv-replay/source-tools.mjs', 'prepare', trace])
(stage/'source-tools.json').write_bytes(manifest)
files = ['inputs.tar', 'source-tools.json']
if trace.stem.endswith('_resume_homepage_zh'):
    wheels = {
        'greenlet-3.5.3-cp310-cp310-manylinux_2_24_x86_64.manylinux_2_28_x86_64.whl': '9bcd2d72ccd70a1ec68ba6ef93e7fbb4420ef9997dabc7010d893bd4015e0bec',
        'playwright-1.61.0-py3-none-manylinux1_x86_64.whl': '54f3b39f6eab832e33458c1dd7da0b5682aedab3b09ae731b5c59fa12fd2024e',
        'pyee-13.0.1-py3-none-any.whl': 'af2f8fede4171ef667dfded53f96e2ed0d6e6bd7ee3bb46437f77e3b57689228',
    }
    files.append('playwright-wheels.tar')
    with tarfile.open(stage/files[-1], 'w') as archive:
        for wheel, digest in wheels.items():
            package, version = wheel.split('-')[:2]
            with urlopen(f'https://pypi.org/pypi/{package}/{version}/json', timeout=30) as response:
                release = next(x for x in json.load(response)['urls'] if x['filename'] == wheel)
            with urlopen(release['url'], timeout=60) as response:
                data = response.read()
            assert sha256(data).hexdigest() == digest == release['digests']['sha256']
            entry = tarfile.TarInfo(wheel)
            entry.size = len(data)
            archive.addfile(entry, BytesIO(data))
(stage/'SHA256SUMS').write_text(''.join(f'{sha256((stage/p).read_bytes()).hexdigest()}  {p}\n' for p in files))
