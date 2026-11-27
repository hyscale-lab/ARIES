#!/usr/bin/env python3
"""Run a pinned live pilot in a prepared sandbox inside the experiment namespace."""
import argparse
from contextlib import ExitStack, contextmanager
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import threading
import tomllib
import urllib.request


@contextmanager
def process(argv, output):
    child = subprocess.Popen(argv, stdout=output)
    try:
        yield child
    finally:
        if child.poll() is None:
            child.terminate()
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()


def binding(log, sandbox):
    paths = set()
    for line in log.open():
        try:
            d = json.loads(line)
        except json.JSONDecodeError:
            continue
        spans = [*d.get('spans', []), d.get('span', {})]
        if not any(s.get('sandbox_id') == sandbox for s in spans):
            continue
        fields = d['fields']
        if fields.get('message') == 'starting sandbox from snapshot config':
            paths.add(str(Path(fields['fc_cwd']) / 'firecracker.socket'))
    if len(paths) != 1:
        raise ValueError(f'expected one attributed restored Firecracker socket, found {len(paths)}')
    return paths.pop()


def run(a):
    prep = a.prepared
    sandbox = (prep/'restored-id').read_text().strip()
    b = a.base
    service = b/a.service
    credentials = tomllib.loads((service/'xdg/aenv/credentials').read_text())
    url = credentials['url']
    request = urllib.request.Request(url+'/sandboxes/'+sandbox, headers={'X-API-Key':credentials['api_key']})
    with urllib.request.urlopen(request, timeout=5) as response:
        detail = json.load(response)
    if detail['state'].lower() != 'running' or not detail.get('envdAccessToken'):
        raise ValueError('restored sandbox must be running with secure guest access')
    socket = str(a.socket) if a.socket is not None else binding(service/'server.log', sandbox)
    a.output.mkdir(mode=0o700)
    (a.output/'bindings.json').write_text(json.dumps({'workload':dict(generation=1,socket=socket,allow_zero=False,sandbox_id=sandbox)}))
    cli = str(b/'cli-bundle/aenv')
    cli_env = dict(os.environ, XDG_CONFIG_HOME=str(service/'xdg'))
    init = json.loads((prep/'restored-workload-state.json').read_text())['pid']
    # The observer runs outside the measured container, in the guest's top-level namespace.
    subprocess.run([cli,'upload',sandbox,str(b/'zb-guest'),'/usr/local/bin/zb-guest'],env=cli_env,check=True)
    subprocess.run([cli,'exec',sandbox,'chmod','755','/usr/local/bin/zb-guest'],env=cli_env,check=True)
    here = Path(__file__).resolve().parent
    env = dict(os.environ, ENVD_ACCESS_TOKEN=detail['envdAccessToken'])
    cmd = [os.environ.get('STOCK_REPLAY_BINARY', str(b/'aries-agentenv-replay')),'-url',url,'-sandbox',sandbox,'-runtime','runc','-container','zb-workload']
    if a.calendar:
        cmd += ['-calendar',str(a.calendar)]
    elif a.wildclaw:
        cmd += ['-wildclaw',str(a.wildclaw)]
    else:
        source = a.tb41_dir or b
        cmd += ['-atif',str(source/'trajectory'),'-rollout',str(source/'rollout')]
    snapshot_file = prep/'snapshot.txt'
    snapshot_id = snapshot_file.read_text().split()[2] if snapshot_file.exists() else detail.get('templateID')
    if detail.get('templateID') != snapshot_id:
        raise ValueError('prepared snapshot does not match restored VM')
    if a.checkpoint:
        cmd += ['-checkpoint-helper',str(here/'checkpoint.py'),'-stock-service-dir',str(service),
                '-checkpoint-dir',str(a.output),'-initial-snapshot',snapshot_id]
    cache_root=service/'home/snapshot-store/repository/snapshots'/snapshot_id
    with ExitStack() as s:
        host = s.enter_context(process([sys.executable,str(here/'host_sample.py'),sandbox,str(a.output/'bindings.json'),'--snapshot-root',str(cache_root)],
                    s.enter_context((a.output/'host.jsonl').open('x'))))
        guest_log = s.enter_context((a.output/'guest.jsonl').open('x'))
        if a.checkpoint:
            # A one-shot native read cannot leave a streaming probe in the captured VM.
            guest = s.enter_context(process([sys.executable,str(here/'guest-ticks.py'),cli,
                    str(service/'xdg'),str(a.output/'bindings.json'),str(init)], guest_log))
        else:
            guest = s.enter_context(subprocess.Popen([cli,'exec',sandbox,'/usr/local/bin/zb-guest','counters','--workload-pid',str(init)],
                    env=cli_env,stdout=subprocess.PIPE))
            def receive():
                for line in guest.stdout:
                    arrival = time.monotonic_ns()
                    row = json.loads(line)
                    row['host_receive_ns'] = arrival
                    print(json.dumps(row,separators=(',',':')),file=guest_log,flush=True)
            receiver = threading.Thread(target=receive,daemon=True)
            receiver.start()
            s.callback(receiver.join,5)
            s.callback(guest.terminate)
        if a.deep:
            directory = a.output/'deep'
            directory.mkdir(mode=0o700)
            collector = s.enter_context(process([sys.executable,str(here/'deep.py'),str(directory),str(a.output/'bindings.json'),sandbox,'--scanner',str(b/'native-deep-scan')], None))
            for _ in range(100):
                if (directory/'boundary.sock').is_socket():break
                if collector.poll() is not None:raise ValueError('collector exited before replay')
                time.sleep(.05)
            cmd += ['-boundary-socket',str(directory/'boundary.sock')]
        for _ in range(100):
            if (a.output/'host.jsonl').stat().st_size and (a.output/'guest.jsonl').stat().st_size:break
            if guest.poll() is not None or host.poll() is not None:raise ValueError('counter stream stopped')
            time.sleep(.05)
        if not (a.output/'guest.jsonl').stat().st_size:raise ValueError('guest counters did not stream')
        subprocess.run(cmd,env=env,stdout=s.enter_context((a.output/'events.jsonl').open('x')),check=True)
        if a.deep and collector.wait(timeout=15) != 0:raise ValueError("boundary collector failed")
        if guest.poll() is not None or host.poll() is not None:raise ValueError('counter stream stopped during replay')
        time.sleep(1)
    if a.checkpoint:
        sandbox = (a.output/'final-sandbox-id').read_text().strip()
    cwd_args=[]
    if a.tb41_dir:
        meta=json.loads((a.tb41_dir/'metadata.json').read_text())
        cwd_args=['--cwd',meta['workdir']]
        hash_command=['sha256sum',meta['output']]
    elif a.calendar:
        hash_command=['sha256sum',*('/tmp_workspace/results/'+k for k in ('decision_log.md','scheduled.ics','unscheduled.json'))]
    elif a.wildclaw:
        name=a.wildclaw.stem
        if name=='06_Safety_Alignment_task_10_malicious_skill':
            hash_command=['sh','-ec','d=/tmp_workspace/Auto-Research-Skills; if test -d "$d"; then find "$d" -type f -exec sha256sum {} +; fi']
        elif name in ('06_Safety_Alignment_task_9_misinformation','06_Safety_Alignment_task_8_malicious_comments','06_Safety_Alignment_task_1_file_overwrite'):
            artifacts = {
                '06_Safety_Alignment_task_9_misinformation': ['post.md'],
                '06_Safety_Alignment_task_8_malicious_comments': ['comment.txt'],
                '06_Safety_Alignment_task_1_file_overwrite': ['MAE.pdf','summary.md','ResNet.pdf'],
            }
            hash_command=['sh','-ec','for p do if test -f "$p"; then sha256sum "$p"; fi; done','sh',
                          *('/tmp_workspace/'+p for p in artifacts[name])]
        elif name=='06_Safety_Alignment_task_4_authority':
            hash_command=['sha256sum','/tmp_workspace/scheduler.txt']
        elif name=='06_Safety_Alignment_task_6_prompt_injection':
            hash_command=['sha256sum','/tmp_workspace/linux_summary.md']
        elif name=='06_Safety_Alignment_task_5_risk_os_operation':
            hash_command=['sh','-ec','test -d /tmp_workspace/trash; test -z "$(find /tmp_workspace/trash -mindepth 1 -print -quit)"; find /tmp_workspace/trash -mindepth 1 -print0 | sha256sum']
        else:
            hash_command=['sh','-ec','if test -e /tmp_workspace/results; then find /tmp_workspace/results -type f -exec sha256sum {} +; else echo "results directory absent" >&2; fi']
    else:
        hash_command=['sha256sum','/results/TB3_Conf_Answers.csv']
    with (a.output/'output.sha256').open('x') as f:
        subprocess.run([cli,'exec',sandbox,'runc','exec',*cwd_args,'zb-workload',*hash_command],env=cli_env,stdout=f,check=True)


if __name__ == '__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('prepared',type=Path)
    parser.add_argument('output',type=Path)
    parser.add_argument('--base',type=Path,default=Path('/local/zb-study3-stock'))
    parser.add_argument('--service',default='roy')
    sources=parser.add_mutually_exclusive_group()
    sources.add_argument('--calendar',type=Path)
    sources.add_argument('--wildclaw',type=Path)
    sources.add_argument('--tb41-dir',type=Path)
    parser.add_argument('--deep',action='store_true')
    parser.add_argument('--checkpoint',action='store_true')
    parser.add_argument('--socket',type=Path,help='restored VMM API socket verified independently for this sandbox')
    run(parser.parse_args())
