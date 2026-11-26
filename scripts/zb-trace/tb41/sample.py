"""Pair a guest cgroup read with Firecracker CPU, RSS and PSS observations every second."""
import json
import os
from pathlib import Path
import select
import signal
import socket
import struct
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from sample_vmm import process_times

BASE = Path(__file__).resolve().parent
SECOND = 10**9


class Finished(Exception):
    pass


def stop(_sig, _frame):
    raise Finished


def vmm_process(api):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as peer:
        directory = os.open(api.parent, os.O_DIRECTORY)
        try:
            peer.connect(f"/proc/self/fd/{directory}/{api.name}")
        finally:
            os.close(directory)
        pid, _, _ = struct.unpack("3i", peer.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    return pid, Path("/proc") / str(pid)


def read_guest(child, tick, deadline):
    while select.select([child.stdout], [], [], max(0, deadline - time.monotonic_ns()) / SECOND)[0]:
        line = child.stdout.readline()
        if not line:
            raise RuntimeError("guest cgroup sample stream closed")
        record = json.loads(line)
        if record["tick"] < tick:
            continue
        if record["tick"] != tick:
            raise RuntimeError("guest cgroup sample out of order")
        return record
    return None


def sample(api):
    with Path(os.environ["ZB_CGROUP_EVENTS"]).open() as stream:
        create = json.loads(stream.readline())
    if create["event"] != "create_start":
        raise ValueError("missing run identity")
    runtime = Path(os.environ["ZB_TB41_RUNTIME"])
    unit = Path(os.environ["ZB_TB41_UNIT"])
    command = [BASE / "bin/cg-exec", unit / "containerd.sock", runtime / "checkpoints",
               runtime / "shim", unit / "aries-io", create["runtime_id"], BASE / "guest.py"]
    pid, proc = vmm_process(api)
    start, _ = process_times(proc)
    hz = os.sysconf("SC_CLK_TCK")
    signal.signal(signal.SIGTERM, stop)
    membership = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    stats = subprocess.Popen([BASE / "bin/cg-stats", runtime / "shim", create["runtime_id"]],
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    try:
        deadline = time.monotonic_ns()
        tick = 0
        while True:
            time.sleep(max(0, deadline - time.monotonic_ns()) / SECOND)
            row = dict(tick=tick, scheduled_ns=deadline, pid=pid, start_ticks=start)
            guest_begin = time.monotonic_ns()
            stats.stdin.write(f"{tick}\n".encode())
            stats.stdin.flush()
            membership.stdin.write(f"{tick}\n".encode())
            membership.stdin.flush()
            begin = time.monotonic_ns()
            before, _ = process_times(proc)
            rollup = (proc / "smaps_rollup").read_text()
            cpu_begin = time.monotonic_ns()
            after, cpu_ticks = process_times(proc)
            end = time.monotonic_ns()
            assert before == after == start
            sizes = {fields[0]: int(fields[1]) * 1024 for line in rollup.splitlines()
                     if (fields := line.split()) and fields[0] in {"Rss:", "Pss:"} and fields[2:] == ["kB"]}
            assert 0 <= sizes["Pss:"] <= sizes["Rss:"]
            timeout = deadline + (12 * SECOND if tick == 0 else SECOND * 8 // 10)
            guest = read_guest(stats, tick, timeout)
            guest_end = time.monotonic_ns()
            members = read_guest(membership, tick, timeout)
            membership_end = time.monotonic_ns()
            row.update(begin_ns=begin, end_ns=end, pss_bytes=sizes["Pss:"], rss_bytes=sizes["Rss:"],
                       placement=os.environ.get("ZB_TB41_PLACEMENT", "solo"),
                       cpu_ticks=cpu_ticks, ticks_per_second=hz, cpu_observed_ns=(cpu_begin + end) // 2,
                       guest_capture_begin_ns=guest_begin, guest_capture_end_ns=guest_end,
                       guest=guest, membership=members, membership_received_ns=membership_end,
                       status="paired" if guest and "unavailable" not in guest and members else "guest_missing")
            if tick == 0 and (guest is None or members is None):
                raise RuntimeError("no initial workload cgroup sample or membership proof")
            print(json.dumps(row, separators=(",", ":")), flush=True)
            tick += 1
            deadline += SECOND
            while deadline + SECOND // 2 < time.monotonic_ns():
                print(json.dumps(dict(tick=tick, scheduled_ns=deadline, pid=pid, start_ticks=start,
                                      status="missed_tick")), flush=True)
                tick += 1
                deadline += SECOND
    except Finished:
        pass
    finally:
        close_helpers(stats, membership)


def close_helpers(*children):
    failure = []
    for child in children:
        child.stdin.close()
    for child in children:
        try:
            status = child.wait(timeout=4)
        except subprocess.TimeoutExpired:
            child.terminate()
            status = child.wait(timeout=4)
        if status:
            failure.append(status)
    if failure:
        raise RuntimeError(f"guest sampling helper exit statuses {failure}")


if __name__ == "__main__":
    sample(Path(sys.argv[1]))
