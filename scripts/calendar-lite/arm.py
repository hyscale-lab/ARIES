"""Run from the worker pilot directory as root with an already booted, fresh arm disk."""
import json
import os
import socket
import subprocess
import sys
import threading
import time
from pathlib import Path

disk, mode, arm, destination = sys.argv[1:]
out = Path(destination)
out.mkdir()
clock = time.monotonic_ns
events = {}
def event(name):
    events[name] = clock()
    (out/'events.json').write_text(json.dumps(events, indent=2))

def run(*args, **kw):
    return subprocess.check_output(args, text=True, **kw)

def guest(cmd):
    return run('./guest', cmd)

def qmp(command, args=None):
    with socket.socket(socket.AF_UNIX) as s:
        s.settimeout(120)
        s.connect('qmp.sock')
        f = s.makefile('rwb', buffering=0)
        assert 'QMP' in json.loads(f.readline())
        for request in [dict(execute='qmp_capabilities'), dict(execute=command, arguments=args or {})]:
            f.write(json.dumps(request).encode()+b'\n')
            while True:
                response=json.loads(f.readline())
                if 'error' in response: raise ValueError(response)
                if 'return' in response: break
        return response['return']

group = Path('/sys/fs/cgroup/system.slice/zb-calendar-lite-vmm.service')
def sample():
    row=dict(ns=clock(), pss_kib=0, cgroup_bytes=0, file_bytes=0, pids=[])
    try:
        row['cgroup_bytes']=int((group/'memory.current').read_text())
        row['file_bytes']=int(dict(l.split() for l in (group/'memory.stat').read_text().splitlines())['file'])
        for pid in (group/'cgroup.procs').read_text().split():
            p=Path('/proc')/pid/'smaps_rollup'
            kib=int(next(l.split()[1] for l in p.read_text().splitlines() if l.startswith('Pss:')))
            row['pids'].append(int(pid))
            row['pss_kib']+=kib
    except (FileNotFoundError, ProcessLookupError):
        pass
    return row

def sample_loop():
    with (out/'samples.jsonl').open('w') as f:
        while not stop.is_set():
            print(json.dumps(sample()), file=f, flush=True)
            stop.wait(.1)

stop=threading.Event()
thread=threading.Thread(target=sample_loop,daemon=True)
thread.start()
try:
    # The guest reads the exact same profile script in both arms.
    (out/'startup.json').write_text(run('./guest','python3 - '+mode+' startup',input=Path('profile.py').read_text()))
    run('./guest','bash -s',input=Path('session.sh').read_text())
    (out/'session.json').write_text(guest('cat /run/lite-session.json'))
    token=guest('python3 /opt/pilot/token.py').strip()
    assert len(token.split()[1])==64
    before=sample()
    assert before['pss_kib']>0 and before['pids']
    (out/'before.json').write_text(json.dumps(before))
    bytes0=os.stat(disk).st_blocks*512
    event('request')
    if arm=='checkpoint':
        qmp('stop')
        assert qmp('query-status')['running'] is False
        result=qmp('human-monitor-command',{'command-line':'savevm calendar'})
        if result.strip(): raise RuntimeError(result)
        (out/'snapshots.txt').write_text(qmp('human-monitor-command',{'command-line':'info snapshots'}))
        qmp('quit')
        deadline=time.monotonic()+30
        while any(Path(f'/proc/{p}').exists() for p in before['pids']):
            if time.monotonic()>deadline: raise TimeoutError('VMM exit')
            time.sleep(.01)
        assert not any(Path(f'/proc/{p}').exists() for p in before['pids'])
    (out/'snapshot-allocation.json').write_text(json.dumps(dict(allocated_delta_bytes=os.stat(disk).st_blocks*512-bytes0)))
    event('hold_start')
    time.sleep(5)
    event('restore_request')
    if arm=='checkpoint':
        run('./boot.sh',disk,mode,'calendar')
    deadline=time.monotonic()+30
    while True:
        try:
            actual=guest('python3 /opt/pilot/token.py').strip()
            break
        except subprocess.CalledProcessError:
            if time.monotonic()>deadline: raise
    assert actual==token
    event('token_read')
    (out/'token.json').write_text(json.dumps(dict(expected=token,actual=actual)))
    (out/'after.json').write_text(json.dumps(sample()))
    (out/'restored.json').write_text(run('./guest','python3 - '+mode+' restored',input=Path('profile.py').read_text()))
    event('replay_start')
    with (out/'replay.jsonl').open('w') as f:
        subprocess.run([os.environ['ARIES_HARBOR'],'-wildclaw',os.environ['ARIES_LITE_TRACE'],'-wildclaw-mode','deltabox-lite-session'],stdout=f,check=True)
    event('replay_end')
    (out/'mount.txt').write_text(guest('findmnt -T "$(readlink -f /tmp_workspace)" -o FSTYPE,TARGET; cat /run/lite-session.json'))
    (out/'hashes.txt').write_text(guest('cd /tmp_workspace && sha256sum results/*'))
    subprocess.run('tar cz tests | ./guest "cd / && tar xz && bash /tests/test.sh"',shell=True,check=True)
    (out/'reward.json').write_text(guest('cat /logs/verifier/reward.json'))
    assert json.loads((out/'reward.json').read_text())['overall_score']==1
    assert (out/'hashes.txt').read_text()==Path('result-hashes.txt').read_text()
    (out/'disk.json').write_text(json.dumps(dict(before_allocated=bytes0,
        after_allocated=os.stat(disk).st_blocks*512)))
finally:
    stop.set()
    thread.join()
