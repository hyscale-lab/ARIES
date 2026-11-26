"""Run the reviewed parallel batch as one stock persistent Python action."""
import json
import os
import subprocess
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from threading import Barrier


def run_batch(path):
    requests = json.loads(Path(path).read_text())
    gate = Barrier(len(requests))
    env = dict(os.environ, HOME="/root", GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_NOSYSTEM="1", LC_ALL="C.UTF-8")

    def run(request):
        gate.wait()
        dispatch = time.monotonic_ns()
        result = subprocess.run(request["argv"], cwd=request["cwd"], timeout=request["timeout"],
                                capture_output=True, text=True, errors="replace", env=env)
        return {"exit": result.returncode, "stdout": result.stdout, "stderr": result.stderr,
                "worker_dispatch_ns": dispatch, "worker_reply_ns": time.monotonic_ns()}

    if len(requests) == 1:
        results = [run(requests[0])]
    else:
        with ThreadPoolExecutor(max_workers=len(requests)) as pool:
            results = list(pool.map(run, requests))
    print(json.dumps(results))
