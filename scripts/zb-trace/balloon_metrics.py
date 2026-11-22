"""Sample stock balloon device counters on a single restored study VM."""
import argparse
import http.client
import json
import os
from pathlib import Path
import select
import socket
import stat
import struct
import time


def flush(api, pid):
    with socket.socket(socket.AF_UNIX,socket.SOCK_STREAM) as sock:
        sock.settimeout(3)
        directory=os.open(api.parent,os.O_DIRECTORY)
        try:
            sock.connect(f"/proc/self/fd/{directory}/{api.name}")
        finally:
            os.close(directory)
        found,_,_=struct.unpack("3i",sock.getsockopt(socket.SOL_SOCKET,socket.SO_PEERCRED,12))
        if found!=pid:
            raise ValueError("wrong VMM API peer")
        client=http.client.HTTPConnection("localhost")
        client.sock=sock
        client.request("PUT","/actions",'{"action_type":"FlushMetrics"}',{"Content-Type":"application/json"})
        response=client.getresponse()
        if response.status!=204:
            raise OSError(f"flush error {response.status} {response.read()}")


def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument("run",type=Path);ap.add_argument("shim",type=Path)
    args=ap.parse_args()
    samples=args.run/"samples.jsonl"
    deadline=time.monotonic()+240
    while not samples.exists() or not samples.stat().st_size:
        if time.monotonic()>deadline:raise TimeoutError("no restored VMM")
        time.sleep(.2)
    sample=json.loads(samples.open().readline())
    pid=sample["pid"]
    proc=Path("/proc")/str(pid)
    candidates=[p for p in (proc/"fd").iterdir() if "metrics" in os.readlink(p) and stat.S_ISFIFO(p.stat().st_mode)]
    if len(candidates)!=1:raise ValueError(f"expected one metric FIFO, found {len(candidates)}")
    fifo=os.open(candidates[0],os.O_RDONLY|os.O_NONBLOCK)
    api, = [p for p in args.shim.rglob("firecracker.sock") if p.is_socket()]
    pending=b""
    try:
        while proc.exists():
            if int((proc/"stat").read_text().rsplit(")",1)[1].split()[19])!=sample["start_ticks"]:
                break
            try:
                flush(api,pid)
            except (FileNotFoundError,ConnectionRefusedError):
                break
            if select.select([fifo],[],[],2)[0]:
                while True:
                    try:
                        chunk=os.read(fifo,65536)
                    except BlockingIOError:
                        break
                    if not chunk:break
                    pending+=chunk
                while b"\n" in pending:
                    line,pending=pending.split(b"\n",1)
                    r=json.loads(line)
                    print(json.dumps(dict(pid=pid,monotonic_ns=time.monotonic_ns(),balloon=r.get("balloon"),utc_timestamp_ms=r.get("utc_timestamp_ms"))),flush=True)
            time.sleep(2)
    finally:
        os.close(fifo)


if __name__=="__main__":
    main()
