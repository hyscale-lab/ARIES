"""Independently count the verified OCI log layer without reading the measured VM's files."""
import argparse
from collections import Counter
import csv
from datetime import date,timedelta
import hashlib
import io
import json
from pathlib import Path
import re
import runpy
import tarfile
import requests

HERE=Path(__file__).resolve().parent
def main(output):
    p=json.loads((HERE/'provenance.json').read_bytes())['registry_input']
    repo='alexgshaw/log-summary-date-ranges'
    token=requests.get('https://auth.docker.io/token',params={'service':'registry.docker.io','scope':f'repository:{repo}:pull'},timeout=30)
    token.raise_for_status()
    r=requests.get(f'https://registry-1.docker.io/v2/{repo}/blobs/'+p['layer'],headers={'Authorization':'Bearer '+token.json()['token']},timeout=100)
    r.raise_for_status()
    assert 'sha256:'+hashlib.sha256(r.content).hexdigest()==p['layer']
    end=date(2025,8,12)
    periods={'today':end,'last_7_days':end-timedelta(days=6),'last_30_days':end-timedelta(days=29),'month_to_date':end.replace(day=1),'total':None}
    counts=Counter()
    digests={}
    with tarfile.open(fileobj=io.BytesIO(r.content),mode='r:*') as layer:
        for f in layer:
            if not f.isfile():continue
            path='/'+f.name.removeprefix('./')
            m=re.fullmatch(r'/app/logs/(\d{4}-\d{2}-\d{2})_[a-z]+\.log',path)
            if not m:continue
            day=date.fromisoformat(m[1]);data=layer.extractfile(f).read();digests[path]=hashlib.sha256(data).hexdigest()
            for text in data.splitlines():
                severity=re.match(rb'^[0-9-]+ [0-9:]+ \[(INFO|ERROR|WARNING|DEBUG)\] ',text)[1].decode()
                for name,start in periods.items():
                    if start is None or start<=day<=end:counts[name,severity]+=1
    manifest=''.join(f'{digests[k]}  {k}\n' for k in sorted(digests))
    assert hashlib.sha256(manifest.encode()).hexdigest()==p['manifest_sha256'] and len(digests)==p['count']
    verify=runpy.run_path(str(HERE/'verifier.py.txt'))
    expected=verify['EXPECTED_ROWS']
    assert all(counts[k,l]==int(v) for k,l,v in expected)
    assert sum(counts['total',s] for s in ('INFO','ERROR','WARNING','DEBUG'))==p['lines']
    output.mkdir(exist_ok=True,parents=True)
    (output/'input-validation.json').write_text(json.dumps(dict(**p,task_verifier_rows=expected,independently_recomputed_rows=[[k,l,counts[k,l]] for k,l,_ in expected],debug_lines_excluded=counts['total','DEBUG']),indent=2)+'\n')
    print(len(expected),'verified CSV rows')
if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('output',type=Path);main(parser.parse_args().output)
