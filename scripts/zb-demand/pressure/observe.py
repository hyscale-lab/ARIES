#!/usr/bin/env python3
"""Run matched file-fill, cold, and reread phases with one-second host PSS."""
import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import time

TRACE = Path(__file__).resolve().parents[2] / "zb-trace"
sys.path.insert(0, str(TRACE))
from idle_hook import Firecracker  # noqa: E402


TICK = 1_000_000_000


def request(vsock, action):
    begin = time.monotonic_ns()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(180)
        directory = os.open(vsock.parent, os.O_DIRECTORY)
        try:
            conn.connect(f"/proc/self/fd/{directory}/{vsock.name}")
        finally:
            os.close(directory)
        with conn.makefile("rb") as stream:
            conn.sendall(b"CONNECT 4102\n")
            if not stream.readline(128).startswith(b"OK "):
                raise RuntimeError("guest pressure fixture connection rejected")
            conn.sendall(action.encode() + b"\n")
            payload = stream.readline(16384)
    end = time.monotonic_ns()
    if not payload.endswith(b"\n"):
        raise RuntimeError("incomplete guest pressure reply")
    result = json.loads(payload)
    if "error" in result:
        raise RuntimeError(result["error"])
    return {"action": action, "host_begin_ns": begin, "host_end_ns": end, "guest": result}


def record(stream, event, **values):
    row = {"event": event, **values}
    stream.write(json.dumps(row, separators=(",", ":")) + "\n")
    stream.flush()
    return row


def check_balloon(api):
    cfg = api.request("GET", "/balloon")
    if cfg["amount_mib"] != 0 or cfg.get("free_page_reporting") is not True:
        raise ValueError(f"unexpected restored balloon configuration {cfg}")
    return cfg


def check_counters(row, variant):
    enabled = row["guest"]["damon"]["enabled"]
    if enabled != ("N" if variant == "native-v2" else "Y"):
        raise ValueError(f"{variant}: unexpected DAMON state {enabled}")


def wait_first_sample(path, process):
    until = time.monotonic() + 12
    while not path.stat().st_size:
        if process.poll() is not None or time.monotonic() >= until:
            raise RuntimeError("no one-second host sample")
        time.sleep(0.05)


def experiment(api, vsock, out, size, variant, result, runtime):
    path = result / "samples.jsonl"
    with path.open("x") as samples, (result / "sampler.log").open("x") as sd:
        sampler = subprocess.Popen([sys.executable, str(TRACE / "sample_vmm.py"), str(api.path)],
                                   stdout=samples, stderr=sd)
        try:
            wait_first_sample(path, sampler)
            # Read stock balloon counters from the same verified VMM.
            config = check_balloon(api)
            record(out, "restored", runtime=runtime, variant=variant, size=size, host_ns=time.monotonic_ns(),
                   balloon=config, machine=api.request("GET", "/machine-config"))
            initial = request(vsock, "stats")
            check_counters(initial, variant)
            record(out, "pre_fill", **initial)
            fill = record(out, "file_fill_and_verify", **request(vsock, "fill"))
            idle_start = fill["host_end_ns"]
            deadline = idle_start + 65 * TICK
            record(out, "cold_start", host_ns=idle_start)
            record(out, "post_fill_cache", **request(vsock, "fincore"))
            tick = 0
            while time.monotonic_ns() < deadline:
                row = request(vsock, "stats")
                check_counters(row, variant)
                record(out, "cold_stats", **row)
                tick += 1
                next_sample = min(idle_start + tick * TICK, deadline)
                time.sleep(max(0, next_sample - time.monotonic_ns()) / TICK)
            record(out, "pre_reread_cache", **request(vsock, "fincore"))
            record(out, "pre_reread_stats", **request(vsock, "stats"))
            reread = record(out, "file_reread", **request(vsock, "reread"))
            record(out, "cold_end", host_ns=reread["host_begin_ns"])
            record(out, "post_reread_cache", **request(vsock, "fincore"))
            record(out, "post_reread_stats", **request(vsock, "stats"))
            if fill["guest"]["sha256"] != reread["guest"]["sha256"]:
                raise ValueError("fill and cold-read checksums differ")
            record(out, "verified", host_ns=time.monotonic_ns())
            time.sleep(1.2)
        finally:
            sampler.terminate()
            sampler.wait(timeout=10)


def run(args):
    outpath = Path(os.environ["ZB_PRESSURE_RUN"])
    size = os.environ["ZB_PRESSURE_SIZE"]
    variant = os.environ["ZB_PRESSURE_VARIANT"]
    api = Firecracker(args.socket)
    api.inspect()
    vsock = api.path.with_name("firecracker.vsock")
    with (outpath / "phases.jsonl").open("a") as out:
        if args.phase == "prepare":
            record(out, "prepare", **request(vsock, f"prepare-{size}"))
        else:
            try:
                experiment(api, vsock, out, size, variant, outpath, args.runtime)
            finally:
                request(vsock, "close")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("prepare", "measure"))
    parser.add_argument("runtime")
    parser.add_argument("socket")
    run(parser.parse_args())
