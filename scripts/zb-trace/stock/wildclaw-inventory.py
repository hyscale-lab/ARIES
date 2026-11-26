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
    calls={}; replies={}; batches=0; has_url=False; external_clients=False; model_in_exec=0
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
                    model_in_exec+=bool(re.search(r'openrouter|OPENAI_API|api\.openai|responses\.create|chat\.completions|image_model',cmd,re.I))
                    has_url|=bool(re.search(r'https?://',cmd))
                    external_clients|=bool(re.search(r'\b(curl|wget|requests|httpx|urlopen|playwright|openrouter)\b',cmd,re.I))
        elif m.get('role')=='toolResult':
            assert m['toolCallId'] not in replies
            replies[m['toolCallId']]=m
    assert replies.keys()<=calls.keys()
    tools=Counter(calls.values())
    running=sum(r.get('details',{}).get('status')=='running' for r in replies.values() if isinstance(r.get('details'),dict))
    search_providers=Counter()
    for reply in replies.values():
        if reply.get('toolName')=='web_search':
            content=reply['content']
            assert len(content)==1 and content[0]['type']=='text'
            payload=json.loads(content[0]['text'])
            search_providers[payload.get('provider','unknown')]+=1
    image_replies=[r for r in replies.values() if r.get('toolName')=='image']
    rejected=sum(isinstance(r.get('details'),dict) and str(r['details'].get('error','')).startswith('Model does not support images:') for r in image_replies)
    image_errors=sum(isinstance(r.get('details'),dict) and r['details'].get('status')=='error' for r in image_replies)
    results.append(dict(**src,calls=len(calls),source_replies=len(replies),unanswered=len(calls)-len(replies),
                        parallel_batches=batches,running_replies=running,tools=json.dumps(dict(sorted(tools.items()))),
                        exec_literal_http_url=has_url,exec_network_client_spelling=external_clients,exec_model_provider_spelling=model_in_exec,
                        search_reply_providers=json.dumps(dict(sorted(search_providers.items()))),image_source_model_input_rejections=rejected,image_source_other_status_errors=image_errors-rejected,image_source_without_error_status=len(image_replies)-image_errors))
with a.output.open('w') as f:
    w=csv.DictWriter(f,fieldnames=results[0],lineterminator='\n');w.writeheader();w.writerows(results)
