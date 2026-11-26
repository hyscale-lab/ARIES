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


def run_interrupt_cases():
    helper = [sys.executable, "-I", "-S", str(HELPER)]
    live = "import os, signal, time; signal.signal(signal.SIGINT, signal.SIG_DFL); print(os.getpgrp(), flush=True); time.sleep(60)"
    child_groups = {}
    leaders = {}
    try:
        for name, source in [("target", live), ("sibling", live), ("canceled", live), ("ended", "import sys; sys.exit(7)")]:
            token = secrets.token_hex(16)
            launch = subprocess.Popen([*helper, "start-interruptible", token, sys.executable, "-c", source], stdout=subprocess.PIPE, text=True)
            child_groups[name] = (token, launch)
            if name != "ended":
                leaders[name] = int(launch.stdout.readline())
        token, target = child_groups["target"]
        subprocess.run([*helper, "interrupt", token], timeout=5, check=True)
        assert target.wait(timeout=5) == 130
        assert child_groups["sibling"][1].poll() is None
        subprocess.run([*helper, "release", token], timeout=5, check=True)
        assert subprocess.run([*helper, "interrupt", token], timeout=5, capture_output=True).returncode != 0
        del child_groups["target"]
        target.stdout.close()
        token, ended = child_groups["ended"]
        assert ended.wait(timeout=5) == 7
        subprocess.run([*helper, "interrupt", token], timeout=5, check=True)
        assert ended.returncode == 7
        token, sibling = child_groups["sibling"]
        subprocess.run([*helper, "release", token], timeout=5, check=True)
        assert sibling.wait(timeout=5) == 137
        del child_groups["sibling"]
        sibling.stdout.close()
        token, canceled = child_groups["canceled"]
        canceled.terminate()
        assert canceled.wait(timeout=5) == -signal.SIGTERM
        subprocess.run([*helper, "release", token], timeout=5, check=True)
        del child_groups["canceled"]
        canceled.stdout.close()
        assert not Path(f"/proc/{leaders['canceled']}").exists()
    finally:
        for token, proc in child_groups.values():
            if proc.poll() is None:
                subprocess.run([*helper, "interrupt", token], timeout=5, check=True)
                proc.wait(timeout=5)
            subprocess.run([*helper, "release", token], timeout=5, check=True)
            proc.stdout.close()


def main():
    assert ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) == 0
    results = [run_case(False), run_case(True)]
    run_interrupt_cases()
    # Adopt and drain the test-only descendants, including the detached control.
    while True:
        try:
            os.waitpid(-1, 0)
        except ChildProcessError:
            break
    print(json.dumps(results, indent=2))


if __name__ == "__main__":
    main()
