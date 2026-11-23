"""Read a restored Firecracker VM's balloon configuration and guest counters."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import time


def capture(socket, output, config_output=None):
    os.chdir(socket.parent)
    inode=socket.stat().st_ino
    if config_output is not None:
        config = subprocess.run(["curl", "-fsS", "--max-time", "3", "--unix-socket", socket.name,
                                 "http://localhost/vm/config"], capture_output=True, check=True, text=True)
        with config_output.open("x") as stream:
            json.dump(json.loads(config.stdout), stream, indent=2)
            stream.write("\n")
    with output.open("x") as stream:
        while True:
            try:
                if socket.stat().st_ino != inode:
                    break
            except FileNotFoundError:
                break
            row=dict(begin_ns=time.monotonic_ns(),socket_directory=socket.parent.name,socket_inode=inode)
            for path in ("balloon","balloon/statistics"):
                result=subprocess.run(["curl","-sS","--max-time","3","--unix-socket",socket.name,
                                       "-w","\n%{http_code}","http://localhost/"+path],capture_output=True,text=True)
                if result.returncode:
                    row["terminal_read_error"]=result.stderr.strip()
                    break
                body,status=result.stdout.rsplit("\n",1)
                row[path]=dict(status=int(status),response=json.loads(body))
            row["end_ns"]=time.monotonic_ns()
            print(json.dumps(row),file=stream,flush=True)
            if "terminal_read_error" in row:
                break
            time.sleep(2)


def restored_socket(run, shim):
    deadline = time.monotonic() + 360
    while time.monotonic() < deadline:
        if (run / "samples.jsonl").is_file():
            with (run / "samples.jsonl").open() as stream:
                sample = stream.readline()
            if sample.endswith("\n"):
                with (run / "events.jsonl").open() as stream:
                    create = json.loads(stream.readline())
                runtime = create["runtime_id"]
                if create["event"] != "create_start" or not re.fullmatch(r"aries-torch-tensor-parallelism-\d+", runtime):
                    raise ValueError("unexpected restored PyTorch runtime identity")
                socket = shim / f"aries#{runtime}" / "firecracker.sock"
                if not socket.is_socket():
                    raise ValueError("restored Firecracker socket is unavailable")
                return socket
        time.sleep(0.1)
    raise TimeoutError("No complete restored VMM sample for balloon read-back")


if __name__=="__main__":
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("socket",type=Path)
    p.add_argument("output",type=Path)
    p.add_argument("--restored-run", type=Path, help="Resolve the restored socket from this run and the shim directory")
    p.add_argument("--vm-config", type=Path, help="Write the restored VM configuration once")
    a=p.parse_args()
    location = a.socket.resolve(strict=True)
    socket = restored_socket(a.restored_run.resolve(), location) if a.restored_run else location
    capture(socket, a.output.resolve(), a.vm_config.resolve() if a.vm_config else None)
