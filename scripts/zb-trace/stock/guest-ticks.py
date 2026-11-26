"""Guest observations across stock restores, fixed host deadlines and explicit gaps."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time


def sample(cli, xdg, binding, pid):
    deadline = time.monotonic_ns()
    while True:
        time.sleep(max(0, deadline-time.monotonic_ns())/1e9)
        begin = time.monotonic_ns()
        row = {'host_request_ns':begin,'host_scheduled_ns':deadline}
        try:
            state = json.loads(binding.read_bytes())['workload']
            row['generation'] = state['generation']
            if state['allow_zero'] or not state['socket']:
                row['unavailable'] = 'suspended or replacement in progress'
            else:
                result = subprocess.run([cli,'exec',state['sandbox_id'],'/usr/local/bin/zb-guest','counters',
                                         '--workload-pid',pid,'--once'],capture_output=True,timeout=1.5,
                                        env=dict(os.environ,XDG_CONFIG_HOME=xdg))
                if result.returncode:
                    row['unavailable'] = 'guest command returned '+str(result.returncode)
                elif state != json.loads(binding.read_bytes())['workload']:
                    row['unavailable'] = 'guest generation changed during sample'
                else:
                    row.update(json.loads(result.stdout))
        except (ValueError,OSError,KeyError,subprocess.TimeoutExpired) as exc:
            row['unavailable'] = type(exc).__name__
        row['host_receive_ns'] = time.monotonic_ns()
        row['missed_host_ticks'] = (row['host_receive_ns'] - deadline)//1_000_000_000
        print(json.dumps(row,separators=(',',':')),flush=True)
        deadline += (1 + row['missed_host_ticks'])*1_000_000_000


if __name__ == '__main__':
    cli, xdg, binding, pid = sys.argv[1:]
    sample(cli, xdg, Path(binding), pid)
