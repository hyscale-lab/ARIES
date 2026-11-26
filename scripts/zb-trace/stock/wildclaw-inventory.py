#!/usr/bin/env python3
"""Inspect original OpenClaw call bindings, without executing recorded commands."""
import argparse
from collections import Counter
import csv
import hashlib
import json
from pathlib import Path
import re

p=argparse.ArgumentParser(description=__doc__)
p.add_argument('manifest',type=Path)
p.add_argument('traces',type=Path)
p.add_argument('video',type=Path)
p.add_argument('output',type=Path)
a=p.parse_args()
results=[]
for src in csv.DictReader(a.manifest.open()):
    path=a.video if src['model']=='Grok 4.5' else a.traces/(src['workload']+'.jsonl')
    raw=path.read_bytes()
    assert hashlib.sha256(raw).hexdigest()==src['sha256']
    calls={}; replies={}; batches=0; has_url=False; external_clients=False
    for line in raw.splitlines():
        m=json.loads(line).get('message',{})
        if m.get('role')=='assistant':
            group=[c for c in m.get('content',[]) if isinstance(c,dict) and c.get('type')=='toolCall']
            batches+=len(group)>1
            for c in group:
                assert c['id'] not in calls
                calls[c['id']]=c['name']
                if c['name']=='exec':
                    cmd=c['arguments'].get('command','')
                    has_url|=bool(re.search(r'https?://',cmd))
                    external_clients|=bool(re.search(r'\b(curl|wget|requests|httpx|urlopen|playwright|openrouter)\b',cmd,re.I))
        elif m.get('role')=='toolResult':
            assert m['toolCallId'] not in replies
            replies[m['toolCallId']]=m
    assert replies.keys()<=calls.keys()
    tools=Counter(calls.values())
    running=sum(r.get('details',{}).get('status')=='running' for r in replies.values() if isinstance(r.get('details'),dict))
    results.append(dict(**src,calls=len(calls),source_replies=len(replies),unanswered=len(calls)-len(replies),
                        parallel_batches=batches,running_replies=running,tools=json.dumps(dict(sorted(tools.items()))),
                        exec_literal_http_url=has_url,exec_network_client_spelling=external_clients))
with a.output.open('w') as f:
    w=csv.DictWriter(f,fieldnames=results[0],lineterminator='\n');w.writeheader();w.writerows(results)
