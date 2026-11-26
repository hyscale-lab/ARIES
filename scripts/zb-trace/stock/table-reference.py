"""Rebuild the table reference hashes from the checked arXiv source and historical byte sizes."""
import hashlib,json,re,sys,tarfile
from pathlib import Path

archive,trace=map(Path,sys.argv[1:])
sha=lambda b:hashlib.sha256(b).hexdigest()
archive_sha=sha(archive.read_bytes())
source_sha=sha(trace.read_bytes())
assert archive_sha=='ddf9b00a44b840413befe79d23ded7a206b08dd12b67384929c8a1973af8ad6f'
assert source_sha=='aa69da99ce7d25f0e9e591183a395ad1538c121e5a7d23cf3b8d1bc781c13502'
with tarfile.open(archive) as t:
    paper=t.extractfile('paper.tex').read().decode()
tables=[]
start=None
lines=paper.splitlines(keepends=True)
for i,line in enumerate(lines):
    code=re.split(r'(?<!\\)%',line,maxsplit=1)[0]
    if r'\begin{table}' in code:
        assert start is None
        start=i
    if r'\end{table}' in code:
        assert start is not None
        tables.append(''.join(lines[start:i+1]).encode())
        start=None
assert len(tables)==19 and start is None
messages=[r['message'] for r in map(json.loads,trace.open()) if 'message' in r]
receipt,=[m['content'][0]['text'] for m in messages if m['role']=='toolResult' and m['content'][0].get('text','').startswith('extracted 19 tables')]
sizes={int(i):int(n) for i,n in re.findall(r'^(\d+)\.tex: (\d+) bytes$',receipt,re.M)}
assert sizes=={i:len(t) for i,t in enumerate(tables,1)}
print(json.dumps(dict(source_url='https://arxiv.org/e-print/2501.07888',source_sha256=archive_sha,
    source_trace_sha256=source_sha,output_sha256={f'{i}.tex':sha(t) for i,t in enumerate(tables,1)}),indent=2))
