"""Regression test for source-compatible guest shell and detached child lifetimes."""

import ctypes
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import sys
import time

HELPER = Path(__file__).with_name("codex_group.py")
SCRIPT = """
sleep 60 </dev/null >/dev/null 2>&1 & ordinary=$!
nohup sleep 60 </dev/null >/dev/null 2>&1 & nohup_pid=$!
setsid sleep 60 </dev/null >/dev/null 2>&1 & detached=$!
printf '%s %s %s %s\n' "$$" "$ordinary" "$nohup_pid" "$detached"
printf 'shell stderr\n' >&2
exit 7
"""


def run_case(profile):
    token = secrets.token_hex(16)
    cmd = ["/bin/bash", "-c", SCRIPT]
    helper = [sys.executable, "-I", "-S", str(HELPER)]
    if profile:
        cmd = [*helper, "start", token, *cmd]
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=10, start_new_session=True)
    assert result.returncode == 7 and result.stderr == "shell stderr\n", result
    shell, ordinary, nohup, detached = map(int, result.stdout.split())
    children = [ordinary, nohup, detached]
    cleanup = list(children)
    try:
        # Independently wait for the detached child to exec before reading SID.
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            if os.getsid(detached) == detached:
                break
            time.sleep(0.001)
        assert os.getsid(detached) == detached
        assert all(os.getpgid(pid) == shell for pid in (ordinary, nohup))
        before = [Path(f"/proc/{pid}").exists() for pid in children]
        if profile:
            subprocess.run([*helper, "release", token], timeout=10, check=True)
            cleanup = [detached]
        after = [Path(f"/proc/{pid}").exists() for pid in children]
        assert before == [True, True, True]
        assert after == ([False, False, True] if profile else [True, True, True])
        return {"mode": "codex_0_151_process_group" if profile else "direct_shell", "shell_exit": result.returncode,
                "children": ["ordinary", "nohup", "setsid"], "alive_before_handle_release": before, "alive_after_handle_release": after}
    finally:
        for pid in cleanup:
            try:
                os.kill(pid, signal.SIGTERM)
            except ProcessLookupError:
                pass


def main():
    assert ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) == 0
    results = [run_case(False), run_case(True)]
    # Adopt and drain the test-only descendants, including the detached control.
    while True:
        try:
            os.waitpid(-1, 0)
        except ChildProcessError:
            break
    print(json.dumps(results, indent=2))


if __name__ == "__main__":
    main()
