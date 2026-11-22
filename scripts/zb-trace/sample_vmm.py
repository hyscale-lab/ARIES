"""Sample one Firecracker process through the peer identity of its API socket."""
import argparse
import json
import os
from pathlib import Path
import socket
import struct
import sys
import time


def process_times(proc):
    # comm may contain spaces or a closing parenthesis.
    fields = (proc / "stat").read_text().rsplit(")", 1)[1].split()
    return int(fields[19]), int(fields[11]) + int(fields[12])


def sample(api_socket, limit, output):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as peer:
        path = Path(api_socket)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try:
            peer.connect(f"/proc/self/fd/{directory}/{path.name}")
        finally:
            os.close(directory)
        pid, _, _ = struct.unpack("3i", peer.getsockopt(
            socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i")))
        proc = Path("/proc") / str(pid)
        identity, _ = process_times(proc)
    frequency = os.sysconf("SC_CLK_TCK")
    previous = None
    deadline = time.monotonic_ns()
    index = 0
    while limit is None or index < limit:
        time.sleep(max(0, deadline - time.monotonic_ns()) / 1e9)
        begin = time.monotonic_ns()
        before, _ = process_times(proc)
        rollup = (proc / "smaps_rollup").read_text()
        after, ticks = process_times(proc)
        end = time.monotonic_ns()
        if before != identity or after != identity:
            raise RuntimeError("VMM process identity changed")
        pss, = [line.split()[1:] for line in rollup.splitlines() if line.startswith("Pss:")]
        if pss[1] != "kB":
            raise ValueError("unexpected procfs PSS unit")
        cpu = None
        if previous is not None:
            last_end, last_ticks = previous
            cpu = (ticks - last_ticks) * 1e9 / (frequency * (end - last_end))
        print(json.dumps(dict(schema=1, pid=pid, start_ticks=identity,
                              scheduled_ns=deadline, begin_ns=begin, end_ns=end,
                              pss_bytes=int(pss[0])*1024, cpu_cores=cpu,
                              cpu_ticks=ticks, ticks_per_second=frequency)), file=output, flush=True)
        previous = end, ticks
        index += 1
        # Preserve missed deadlines as gaps and avoid collecting catch-up bursts.
        deadline += ((end - deadline) // 1_000_000_000 + 1) * 1_000_000_000


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("api_socket")
    parser.add_argument("--count", type=int)
    args = parser.parse_args()
    if args.count is not None and args.count < 1:
        parser.error("--count must be positive")
    sample(args.api_socket, args.count, sys.stdout)
