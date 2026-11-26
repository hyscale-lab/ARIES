#!/usr/bin/env python3
"""Fetch one pinned Harbor environment, checking Git or LFS object identities."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import time
from urllib.parse import quote
from urllib.error import HTTPError
from urllib.request import urlopen

dataset='InternLM/WildClawBench-Harbor'
revision='c880033a1bd4023ffe0175bb2983b46719a7e36b'
def fetch(task, target):
    if not re.fullmatch(r'[A-Za-z0-9_]+',task):
        raise ValueError('invalid task')
    prefix=task+'/environment/'
    entries=[]
    url=f'https://huggingface.co/api/datasets/{dataset}/tree/{revision}/{prefix}?recursive=true&limit=1000'
    while url:
        with urlopen(url,timeout=90) as response:
            entries.extend(json.load(response))
            nxt=re.search(r'<([^<>]+)>;\s*rel="next"',response.headers.get('Link',''))
            url=nxt[1] if nxt else None
            if url and not url.lower().startswith(f'https://huggingface.co/api/datasets/{dataset}/tree/{revision}/'.lower()):
                raise ValueError('unexpected pagination target')
    def save(entry):
        name=entry['path'];rel=PurePosixPath(name.removeprefix(prefix))
        if not name.startswith(prefix) or '..' in rel.parts or rel.is_absolute():
            raise ValueError('unexpected publisher path')
        path=target/rel
        if entry['type']=='directory':
            path.mkdir(parents=True,exist_ok=True)
            return
        assert entry['type']=='file'
        path.parent.mkdir(parents=True,exist_ok=True)
        def valid(candidate):
            if candidate.stat().st_size != entry['size']:
                return False
            lfs = entry.get('lfs')
            digest = hashlib.sha256() if lfs else hashlib.sha1(('blob '+str(entry['size'])).encode()+b'\0')
            expected = lfs['oid'] if lfs else entry['oid']
            with candidate.open('rb') as source:
                for chunk in iter(lambda: source.read(1 << 20), b''):
                    digest.update(chunk)
            return digest.hexdigest() == expected
        if path.is_file() and valid(path):
            return
        tmp=path.with_name(path.name+'.download')
        for attempt in range(7):
            try:
                with urlopen(f'https://huggingface.co/datasets/{dataset}/resolve/{revision}/'+quote(name,safe='/'),timeout=90) as f, tmp.open('wb') as out:
                    shutil.copyfileobj(f,out,length=1 << 20)
                if not valid(tmp):
                    raise ValueError('publisher object mismatch: '+name)
                tmp.replace(path)
                return
            except OSError as exc:
                if attempt==6:raise
                delay=2**attempt
                if isinstance(exc, HTTPError) and exc.code==429:
                    retry=exc.headers.get("Retry-After","")
                    delay=max(delay, int(retry) if retry.isdigit() else 62)
                time.sleep(delay)
    with ThreadPoolExecutor(max_workers=4) as pool:list(pool.map(save,entries))
    return dict(revision=revision,task=task,files=sum(e['type']=='file' for e in entries),bytes=sum(e.get('size',0) for e in entries if e['type']=='file'))
if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('task');parser.add_argument('target',type=Path)
    print(json.dumps(fetch(**vars(parser.parse_args()))))
