"""Crab guest-side pilot operations. Run with Crab's installed Python as root."""
import json
from pathlib import Path
import shlex
import subprocess
import sys

from crab import Engine, Sandbox

SERVER = """import os,secrets,socket
token=secrets.token_hex(24)
s=socket.socket(socket.AF_UNIX)
s.bind('/tmp/crab-token')
s.listen()
while True:
    c,_=s.accept()
    with c: c.sendall(f'{os.getpid()}:{token}'.encode())
"""
CLIENT = """import socket
s=socket.socket(socket.AF_UNIX)
s.connect('/tmp/crab-token')
print(s.recv(256).decode())
"""


def main():
    command, name, *args = sys.argv[1:]
    with Engine.connect(timeout_seconds=300) as engine:
        if command == "create":
            sandbox = Sandbox(image="calendar-pilot:local", name=name, engine=engine)
            subprocess.run(["runc", "--root", "/var/lib/crab/runtime/runtime-state", "update", "--memory", "536870912",
                            "--cpu-quota", "100000", "--cpu-period", "100000", str(sandbox.sandbox_id)], check=True)
            state = json.loads(subprocess.check_output(["runc", "--root", "/var/lib/crab/runtime/runtime-state", "state", str(sandbox.sandbox_id)]))
            rel = Path(f"/proc/{state['pid']}/cgroup").read_text().strip().split("::")[1].lstrip("/")
            parent = (Path("/sys/fs/cgroup") / rel).parent
            assert parent.name == "crab-sdk"
            (parent / "memory.max").write_text("536870912")
            (parent / "cpu.max").write_text("100000 100000")
            sandbox.commands.run("nohup python3 -c " + shlex.quote(SERVER) +
                                 " </dev/null >/tmp/token.log 2>&1 &", check=True)
            print(sandbox.sandbox_id)
            return
        sandbox = Sandbox.connect(name, engine=engine)
        if command == "group":
            state = json.loads(subprocess.check_output(["runc", "--root", "/var/lib/crab/runtime/runtime-state", "state", name]))
            rel = Path(f"/proc/{state['pid']}/cgroup").read_text().strip().split("::")[1].lstrip("/")
            group = Path("/sys/fs/cgroup") / rel
            assert group.name == name and (group / "cgroup.procs").read_text().strip()
            assert (group.parent / "memory.max").read_text().strip() == "536870912"
            assert (group.parent / "cpu.max").read_text().strip() == "100000 100000"
            print(group)
        elif command == "token":
            print(sandbox.commands.run(["python3", "-c", CLIENT], check=True).stdout.strip())
        elif command == "checkpoint":
            ckpt = sandbox.checkpoint(leave_running=False)
            print(ckpt)
        elif command == "restore":
            sandbox.restore(args[0])
        elif command == "kill":
            sandbox.kill()
        elif command == "verify":
            pid = json.loads(subprocess.check_output(["runc", "--root", "/var/lib/crab/runtime/runtime-state", "state", name]))["pid"]
            path = Path(f"/proc/{pid}/root")
            subprocess.run(["cp", "-a", "--no-preserve=ownership", "/var/tmp/fixture/tests", str(path / "tests")], check=True)
            sandbox.commands.run(["bash", "/tests/test.sh"], check=True)
            result = json.loads(sandbox.files.read("/logs/verifier/reward.json"))
            hashes = sandbox.commands.run(["bash", "-c", "sha256sum results/*"], cwd="/tmp_workspace", check=True).stdout
            print(json.dumps({"reward": result, "hashes": hashes}))
            assert len(result) == 17 and all(value == 1 for value in result.values())
            assert hashes.strip() == Path("/var/tmp/fixture/result-hashes.txt").read_text().strip()
        elif command == "exec":
            cwd, operation, payload = args
            argv = ["cat", payload] if operation == "ReadFile" else ["bash", "-c", payload]
            result = sandbox.commands.run(argv, cwd=cwd or "/root")
            sys.stdout.write(result.stdout)
            sys.stderr.write(result.stderr)
            return result.returncode
        else:
            raise ValueError(command)


if __name__ == "__main__":
    sys.exit(main())
