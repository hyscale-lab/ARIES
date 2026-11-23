"""Read the fixed telemetry response over a Firecracker host-side vsock."""
import argparse
import json
import os
from pathlib import Path
import socket
import time


def snapshot(vsock, request=b"snapshot\n"):
    start=time.monotonic_ns()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(20)
        directory=os.open(vsock.parent,os.O_DIRECTORY)
        try:
            conn.connect(f"/proc/self/fd/{directory}/{vsock.name}")
        finally:
            os.close(directory)
        with conn.makefile("rb") as stream:
            conn.sendall(b"CONNECT 4099\n")
            reply=stream.readline(128)
            if not reply.startswith(b"OK "):
                raise RuntimeError(f"guest telemetry connection rejected: {reply!r}")
            conn.sendall(request)
            payload=stream.readline(64*1024*1024+1)
    end=time.monotonic_ns()
    if not payload.endswith(b"\n") or len(payload)>64*1024*1024:
        raise ValueError("incomplete or excessive guest telemetry")
    return dict(host_begin_ns=start,host_end_ns=end,guest=json.loads(payload))


if __name__=="__main__":
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("vsock", type=Path)
    args=p.parse_args()
    print(json.dumps(snapshot(args.vsock)))
