"""Replay the no-poll Roy recording inside a DeltaBox direct-profile VM."""
import json
import os
from pathlib import Path
import shlex
import sys
import tempfile
import time

sys.path.insert(0, '/repo')
import backends.deltabox.gsd.root_sandbox as rs

# shortcut: Roy has no asynchronous polls; implement persistent process handles before using other recordings.
rs._WORKER_SERVER = rs._WORKER_SERVER.replace('r.stdout[-2000:]', 'r.stdout').replace('r.stderr[-1000:]', 'r.stderr').replace('time.sleep(0.2)', 'time.sleep(0.005)')

class Sandbox(rs.RootOverlaySandbox):
    def exec(self, command, timeout=600):
        done = Path(self.merged) / 'cmddone.txt'
        done.unlink(missing_ok=True)
        (Path(self.merged) / 'cmd.txt').write_text(command)
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if done.exists():
                return done.read_text()
            time.sleep(.005)
        raise TimeoutError('worker command timed out')

def emit(event, **fields):
    print('ZB_EVENT ' + json.dumps(dict(event=event, guest_monotonic_ns=time.monotonic_ns(), **fields)), flush=True)

def run(sandbox, command):
    response = sandbox.exec('cd /app && /bin/bash -lc ' + shlex.quote('export PATH=/opt/venv/bin:/root/.local/bin:"$PATH"; ' + command), timeout=30)
    header, output = response.split('\n', 1)
    assert header.startswith('rc='), response
    return int(header[3:]), output

plan = json.loads(Path('/tmp/plan.json').read_text())
sandbox = Sandbox('/mnt/roy', tempfile.mkdtemp(prefix='zb-roy-', dir='/dev/shm'), python_bin='/opt/venv/bin/python')
sandbox.mount()
try:
    sandbox.launch()
    time.sleep(.2)
    proc = Path(f'/proc/{sandbox.worker_pid}')
    emit('worker_started', pid=sandbox.worker_pid, alive=proc.exists(), cmdline=(proc/'cmdline').read_bytes().decode(errors='replace') if proc.exists() else '')
    readiness = '''set -eu
export LC_ALL=C
test "$(sha256sum data/*.mol data/TB3_Conf_Properties.csv | sha256sum)" = '236aeefd45cca191035b28b385bc8226eca7b4c2bf8fb1d8966bfd7b8f288bb4  -'
test -d /results; test ! -e /results/TB3_Conf_Answers.csv; test ! -e /app/fit_roy.py
if command -v apply_patch; then exit 1; fi
test "$(command -v python)" = /opt/venv/bin/python
python - <<'PYCHECK'
from importlib.metadata import version
from importlib.util import find_spec
import sys
assert sys.version_info[:2] == (3, 11) and find_spec('sklearn') is None
for name, expected in {'pandas':'3.0.3', 'numpy':'2.4.6', 'scipy':'1.17.1', 'rdkit':'2026.3.2'}.items():
    assert version(name) == expected, (name, version(name))
PYCHECK'''
    rc, output = run(sandbox, readiness)
    emit('readiness', rc=rc, output=output)
    assert rc == 0
    # Begin from a prepared checkpoint just like the AgentENV arm.
    emit('initial_checkpoint_start'); sandbox.checkpoint('initial'); emit('initial_checkpoint_end')
    emit('initial_restore_start'); sandbox.restore('initial'); emit('initial_restore_end')
    emit('session_start')
    count = 0
    pending_checkpoint = False
    for event in plan['events']:
        wait_ns = event['modeled_wait_ns']
        deadline = time.monotonic_ns() + wait_ns
        if pending_checkpoint:
            emit('checkpoint_start', after_sequence=count)
            sandbox.checkpoint('wait')
            emit('checkpoint_end', after_sequence=count)
            pending_checkpoint = False
        if wait_ns:
            emit('wait_start', line=event['rollout_line'], requested_ns=wait_ns)
            time.sleep(max(0, (deadline-time.monotonic_ns())/1e9))
            emit('wait_end', line=event['rollout_line'], requested_ns=wait_ns)
        if event['kind'] != 'exec_command':
            continue
        count += 1
        if count == 4:
            emit('restore_start', before_sequence=count)
            sandbox.restore('wait')
            emit('restore_end', before_sequence=count)
        call = event['call_id']
        emit('dispatch', sequence=count, call_id=call)
        rc, output = run(sandbox, plan['calls'][call])
        emit('reply', sequence=count, call_id=call, rc=rc, output=output)
        if count == 3:
            pending_checkpoint = True
    emit('session_end', calls=count)
    assert count == 5
finally:
    sandbox.teardown()
