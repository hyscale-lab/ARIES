#!/usr/bin/env python3
"""Run one synchronized cohort inside the study service mount and network namespaces."""
import argparse
from contextlib import ExitStack
import json
from pathlib import Path
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent


def wait_for(check, workers, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        for worker in workers:
            if worker.poll() is not None:
                raise RuntimeError(f'worker exited before barrier with status {worker.returncode}')
        value = check()
        if value:
            return value
        time.sleep(.1)
    raise TimeoutError('timed out awaiting cohort barrier')


def run(args):
    conf = json.loads(args.config.read_text())
    tasks = conf['tasks']
    if set(tasks) != {'wal-recovery-ordering', 'session-window-debug', 'calendar', 'chapter-summary'}:
        raise ValueError('expected the four selected trajectories')
    args.output.mkdir(mode=0o700)
    gate = [sys.executable, str(HERE / 'cow-gate.py')]
    py = sys.executable
    measure_dir = args.output / 'measure'
    def held():
        records = json.loads(subprocess.check_output([*gate, 'list', conf['gate']]))
        ready = [r for r in records if r['state'] == 'held']
        if len(ready) < 4:
            return None
        bindings = {}
        for name, task in tasks.items():
            vm, = [r for r in ready if task['snapshot'] in Path(r['load']['snapshot_path']).parts]
            bindings[name] = vm
        return bindings

    with ExitStack() as stack:
        def launch(cmd, log):
            err = stack.enter_context((args.output / log).open('x'))
            proc = subprocess.Popen(cmd, stderr=err)
            def close():
                if proc.poll() is None:
                    proc.terminate()
                    proc.wait(timeout=10)
            stack.callback(close)
            return proc
        replays = [launch([py, HERE / 'cow-replay.py', name, task['snapshot'], conf['service'],
                           conf['inputs'], args.output / name], name + '.stderr') for name, task in tasks.items()]
        vms = wait_for(held, replays, 180)
        manifest = dict(ancestor=conf['ancestor'], vms=[
            dict(name=name, socket=vm['real_socket'], setup=tasks[name]['setup']) for name, vm in vms.items()])
        (args.output / 'held.json').write_text(json.dumps(vms, indent=2))
        manifest_path = args.output / 'manifest.json'
        manifest_path.write_text(json.dumps(manifest, indent=2))
        sampler = launch([py, HERE / 'cow-sample.py', 'run', manifest_path, measure_dir,
                          '--interval', str(args.interval), *(['--no-idle'] if args.no_idle else [])], 'sampler.stderr')
        wait_for(lambda: (measure_dir / 'baseline.json').exists(), [*replays, sampler], 180)
        result = subprocess.check_output([*gate, 'release', conf['gate'], *[str(vm['pid']) for vm in vms.values()]])
        (measure_dir / 'released').touch(exist_ok=False)
        (args.output / 'release.json').write_bytes(result)
        deadline = time.monotonic() + 600
        while any(p.poll() is None for p in replays):
            if sampler.poll() is not None or any(p.poll() not in (None, 0) for p in replays):
                raise RuntimeError('sampler or replay failed during measurement')
            if time.monotonic() > deadline:
                raise TimeoutError('cohort replay timeout')
            time.sleep(.1)
        (measure_dir / 'stop').touch(exist_ok=False)
        if sampler.wait(timeout=90) or any(p.returncode for p in replays):
            raise RuntimeError('incomplete measurements')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('config', type=Path)
    parser.add_argument('output', type=Path)
    parser.add_argument('--interval', type=float, default=5)
    parser.add_argument('--no-idle', action='store_true')
    run(parser.parse_args())
