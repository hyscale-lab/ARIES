#!/usr/bin/env python3
"""Restore one ready snapshot through the common gate and immediately replay its recording."""
import argparse
import json
import os
import subprocess
import time
import tomllib
import urllib.request
from pathlib import Path


def run(args):
    args.output.mkdir(mode=0o700)
    credentials = tomllib.loads((args.service / 'xdg/aenv/credentials').read_text())
    url = credentials['url'].rstrip('/')
    cli = args.service.parent / 'cli-bundle/aenv'
    start_ns = time.monotonic_ns()
    env = dict(os.environ, XDG_CONFIG_HOME=str(args.service / 'xdg'))
    sandbox = subprocess.check_output(
        [cli, 'start', '-d', '--timeout', '3600', args.snapshot], env=env, text=True).strip()
    (args.output / 'restored-id').write_text(sandbox + '\n')
    request = urllib.request.Request(f'{url}/sandboxes/{sandbox}', headers={'X-API-Key': credentials['api_key']})
    with urllib.request.urlopen(request, timeout=10) as response:
        detail = json.load(response)
    if detail['templateID'] != args.snapshot or detail['state'].lower() != 'running':
        raise ValueError('restored sandbox identity or state mismatch')
    env['ENVD_ACCESS_TOKEN'] = detail['envdAccessToken']
    cmd = [args.inputs / 'aries-agentenv-replay', '-url', url, '-sandbox', sandbox,
           '-runtime', 'runc', '-container', 'zb-workload']
    if args.task == 'calendar':
        cmd.extend(['-calendar', args.inputs / '01_Productivity_Flow_task_6_calendar_scheduling.jsonl'])
    elif args.task == 'chapter-summary':
        cmd.extend(['-wildclaw', args.inputs / '06_Safety_Alignment_task_6_prompt_injection.jsonl'])
    else:
        cmd.extend(['-atif', args.inputs / args.task / 'trajectory', '-rollout', args.inputs / args.task / 'rollout'])
    launch_ns = time.monotonic_ns()
    with (args.output / 'events.jsonl').open('x') as events:
        subprocess.run(cmd, env=env, stdout=events, check=True)
    receipt = dict(task=args.task, snapshot=args.snapshot, sandbox=sandbox, restore_request_ns=start_ns,
                   runner_launch_ns=launch_ns, runner_exit_ns=time.monotonic_ns())
    (args.output / 'completion.json').write_text(json.dumps(receipt, indent=2) + '\n')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('task', choices=['wal-recovery-ordering', 'session-window-debug', 'calendar', 'chapter-summary'])
    parser.add_argument('snapshot')
    for path in ['service', 'inputs', 'output']:
        parser.add_argument(path, type=Path)
    run(parser.parse_args())
