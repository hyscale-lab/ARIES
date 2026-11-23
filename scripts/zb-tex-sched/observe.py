#!/usr/bin/env python3
"""Arm and stop the guest scheduler trace around the single original tool."""
import json
import os
from pathlib import Path
import socket
import sys
import time

from idle_hook import Firecracker


def request(path, command):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(30)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try:
            conn.connect(f"/proc/self/fd/{directory}/{path.name}")
        finally:
            os.close(directory)
        with conn.makefile("rb") as stream:
            conn.sendall(b"CONNECT 4107\n")
            assert stream.readline(128).startswith(b"OK "), "guest trace vsock unavailable"
            begin = time.monotonic_ns()
            conn.sendall(command.encode() + b"\n")
            response = stream.readline((50 << 20) + 1)
            end = time.monotonic_ns()
    assert response.endswith(b"\n") and len(response) <= 50 << 20
    guest = json.loads(response)
    if "error" in guest:
        raise RuntimeError(guest["error"])
    return {"host_begin_ns": begin, "host_end_ns": end, "guest": guest}


def main():
    phase, runtime, step, socket_path = sys.argv[1:]
    if step != "12" or phase not in {"pre-tool", "post-tool"}:
        return
    api = Firecracker(socket_path)
    api.inspect()
    assert api.vm_id == runtime
    path = api.path.with_name("firecracker.vsock")
    begin = time.monotonic_ns()
    initial = request(path, "arm") if phase == "pre-tool" else None
    prefix = "a" if phase == "pre-tool" else "b"
    markers = [request(path, f"sync {prefix}{i}") for i in range(5)]
    final = request(path, "stop") if phase == "post-tool" else None
    row = dict(phase=phase, step=int(step), runtime_id=runtime, vmm_identity=api.identity,
               hook_begin_ns=begin, hook_end_ns=time.monotonic_ns(), initial=initial,
               markers=markers, final=final)
    with (Path(os.environ["ZB_TEX_SCHED_OUTPUT"]) / (phase + ".json")).open("x") as out:
        json.dump(row, out)
        out.write("\n")


if __name__ == "__main__":
    main()
