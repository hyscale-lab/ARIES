#!/usr/bin/env python3
"""Losslessly gzip completed UFFD fault logs and verify decompressed SHA-256."""
import argparse
import gzip
import hashlib
import json
from pathlib import Path


def digest(stream):
    h=hashlib.sha256()
    while block:=stream.read(1024*1024):h.update(block)
    return h.hexdigest()


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('root',type=Path);a=p.parse_args()
    records=[]
    for source in sorted(a.root.glob('step-*/faults.jsonl')):
        # Do not compact an active/incomplete handler log.
        with source.open('rb') as stream:
            stream.seek(max(0,source.stat().st_size-16384));tail=stream.read().splitlines()
        last=json.loads(tail[-1])
        if last.get('event')!='summary':raise RuntimeError('No terminal handler summary: '+str(source))
        destination=source.with_suffix(source.suffix+'.gz')
        if destination.exists():raise RuntimeError('Archive already exists: '+str(destination))
        h=hashlib.sha256();original_bytes=source.stat().st_size
        with source.open('rb') as src,gzip.open(destination,'xb',compresslevel=1) as dst:
            while block:=src.read(1024*1024):h.update(block);dst.write(block)
        with gzip.open(destination,'rb') as src:
            if digest(src)!=h.hexdigest():raise RuntimeError('Decompressed checksum mismatch')
        destination.chmod(0o600)
        records.append({'original':str(source.relative_to(a.root)),'archive':str(destination.relative_to(a.root)),
            'original_bytes':original_bytes,'archive_bytes':destination.stat().st_size,'original_sha256':h.hexdigest(),
            'decompressed_sha256_verified':True})
        source.unlink()
    (a.root/'compression.json').write_text(json.dumps(records,indent=2)+'\n')
    print(json.dumps({'logs':len(records),'original_bytes':sum(r['original_bytes'] for r in records),'archive_bytes':sum(r['archive_bytes'] for r in records),'sha256_verified':True}))


if __name__=='__main__':main()
