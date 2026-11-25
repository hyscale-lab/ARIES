"""Fetch one selected original task's Docker context from its pinned Git tree."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
from urllib.request import urlopen

REV = '452bf305c6daa62fc59061d22133a7cbc7c1572e'
OWNER = 'harbor-framework/terminal-bench'


def fetch(task, dest):
    prefix = f'tasks/{task}/environment/'
    with urlopen(f'https://api.github.com/repos/{OWNER}/git/trees/{REV}?recursive=1',timeout=90) as response:
        tree = json.load(response)
    if tree['sha'] != REV or tree['truncated']:
        raise ValueError('source tree incomplete')
    entries = [e for e in tree['tree'] if e['path'].startswith(prefix) and e['type'] != 'tree']
    if not any(e['path'] == prefix+'Dockerfile' for e in entries):
        raise ValueError('missing Dockerfile')
    dest.mkdir(parents=True,exist_ok=False)

    def retrieve(e):
        path = e['path']
        local = dest / path.removeprefix(prefix)
        if not local.resolve().is_relative_to(dest.resolve()) or e['mode'] not in ('100644','100755'):
            raise ValueError(f'unsupported source entry {path}')
        with urlopen(f'https://raw.githubusercontent.com/{OWNER}/{REV}/{path}',timeout=90) as response:
            data=response.read()
        if hashlib.sha1(b'blob '+str(len(data)).encode()+b'\0'+data).hexdigest()!=e['sha']:
            raise ValueError(f'Git blob mismatch {path}')
        local.parent.mkdir(parents=True,exist_ok=True)
        local.write_bytes(data)
        local.chmod(int(e['mode'][-3:],8))

    with ThreadPoolExecutor(max_workers=8) as pool:
        list(pool.map(retrieve,entries))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('task')
    p.add_argument('destination',type=Path)
    a=p.parse_args()
    fetch(a.task,a.destination)
