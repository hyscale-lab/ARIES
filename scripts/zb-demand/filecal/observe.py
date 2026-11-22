#!/usr/bin/env python3
"""Corroborate one tracked guest file page in the restored VMM and snapshot."""
import argparse
from contextlib import contextmanager
from hashlib import sha256
import json
import os
from pathlib import Path
import socket
import struct
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "zb-trace"))
from idle_hook import Firecracker, select_slots, snapshot_path  # noqa: E402
from idle import start_identity  # noqa: E402


PAGE = 4096
PRESENT = 1 << 63
PFN_MASK = (1 << 55) - 1
GUEST_DIRTY = 1 << 4
GUEST_WRITEBACK = 1 << 8
MIB = 1 << 20


def guest_request(vsock, action):
    begin = time.monotonic_ns()
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.settimeout(15)
        directory = os.open(vsock.parent, os.O_DIRECTORY)
        try:
            conn.connect(f"/proc/self/fd/{directory}/{vsock.name}")
        finally:
            os.close(directory)
        with conn.makefile("rb") as stream:
            conn.sendall(b"CONNECT 4101\n")
            answer = stream.readline(128)
            if not answer.startswith(b"OK "):
                raise RuntimeError(f"file fixture vsock failed: {answer!r}")
            conn.sendall(action.encode() + b"\n")
            data = stream.readline(4096)
    end = time.monotonic_ns()
    if not data.endswith(b"\n"):
        raise RuntimeError("incomplete guest response")
    reply = json.loads(data)
    if "error" in reply:
        raise RuntimeError(reply["error"])
    return reply, (begin, end)


@contextmanager
def frozen(api, event):
    event["pause_request_ns"] = time.monotonic_ns()
    try:
        api.request("PATCH", "/vm", "Paused")
        event["pause_ack_ns"] = time.monotonic_ns()
        yield
    finally:
        event["resume_request_ns"] = time.monotonic_ns()
        try:
            api.request("PATCH", "/vm", "Resumed")
            event["resume_ack_ns"] = time.monotonic_ns()
        except Exception:
            if api.request("GET", "/")["state"] != "Running":
                raise
            event["running_confirmed_ns"] = time.monotonic_ns()


def read_at(path, size, offset):
    fd = os.open(path, os.O_RDONLY)
    try:
        data = os.pread(fd, size, offset)
        if len(data) != size:
            raise OSError(f"short read from {path}")
        return data
    finally:
        os.close(fd)


def one_page(pid, start_ticks, slot, baseline, guest_pfn):
    gpa = guest_pfn * PAGE
    if slot["guest_phys_addr"] != 0 or not 0 <= gpa <= slot["memory_size"] - PAGE:
        raise ValueError("known page requires a zero-based, fully captured RAM slot")
    hva = slot["userspace_addr"] + gpa
    root = Path("/proc") / str(pid)
    offset = hva // PAGE * 8
    baseline_digest = sha256(read_at(baseline, PAGE, gpa)).hexdigest()
    pte, = struct.unpack("=Q", read_at(root / "pagemap", 8, offset))
    if not pte & PRESENT:
        return {"resident": False, "snapshot_sha256": baseline_digest}
    frame = pte & PFN_MASK
    if not frame:
        raise RuntimeError("resident host PFN is masked or zero")
    live_digest = sha256(read_at(root / "mem", PAGE, hva)).hexdigest()
    later, = struct.unpack("=Q", read_at(root / "pagemap", 8, offset))
    if pte & (PRESENT | PFN_MASK) != later & (PRESENT | PFN_MASK) or start_identity(pid) != start_ticks:
        raise RuntimeError("host mapping or process changed during the comparison")
    return {"resident": True, "snapshot_sha256": baseline_digest, "live_sha256": live_digest,
            "snapshot_equal": baseline_digest == live_digest, "host_pfn_private": frame}


def verify(rows):
    state, read, write, synced = [x["guest"] for x in rows]
    if state["pte_present"] or state["mincore_present"]:
        raise ValueError("restore did not preserve the cold file cache and absent guest PTE")
    if len({(x["device_major"], x["device_minor"], x["inode"], x["file_offset_bytes"]) for x in
            (state, read, write, synced)}) != 1:
        raise ValueError("file ownership or offset changed in the fixture")
    if len({x["guest_pfn"] for x in (read, write, synced)}) != 1:
        raise ValueError("guest PFN changed in the fixture")
    for row in rows[1:]:
        host, guest = row["host"], row["guest"]
        if not host["resident"] or host["live_sha256"] != guest["mapped_sha256"]:
            raise ValueError("host and guest bytes differ for the controlled page")
    if len({x["host"]["snapshot_sha256"] for x in rows[1:]}) != 1:
        raise ValueError("baseline snapshot bytes changed within the fixture")
    if any(row["host"]["snapshot_equal"] for row in rows[1:]):
        raise ValueError("expected different bytes in the same frame of the private snapshot")
    flags = [x["guest_page_flags"] for x in (read, write, synced)]
    if flags[0] & (GUEST_DIRTY | GUEST_WRITEBACK) or flags[2] & (GUEST_DIRTY | GUEST_WRITEBACK):
        raise ValueError("known backed read and post-sync page must be clean")
    if not flags[1] & GUEST_DIRTY or flags[1] & GUEST_WRITEBACK:
        raise ValueError("buffered write did not expose a dirty page before writeback")
    if read["mapped_sha256"] != state["known_synced_backing_sha256"]:
        raise ValueError("read bytes differ from earlier direct backing read")
    if (write["mapped_sha256"] == read["mapped_sha256"]
            or synced["mapped_sha256"] != write["mapped_sha256"]
            or synced["known_synced_backing_sha256"] != synced["mapped_sha256"]):
        raise ValueError("writeback did not preserve the buffered mutation")


def run(args):
    api = Firecracker(args.socket)
    version = api.inspect()["vmm_version"]
    vsock = api.path.with_name("firecracker.vsock")
    if args.phase == "prepare":
        guest, transport = guest_request(vsock, "prepare")
        print(json.dumps(dict(phase="prepare", guest=guest, transport_ns=transport)), flush=True)
        return
    slots, _ = select_slots(
        Path(os.environ["ZB_SLOT_EVENTS"]).read_text(), *api.identity,
        10**9 // os.sysconf("SC_CLK_TCK"), api.request("GET", "/machine-config")["mem_size_mib"] * MIB,
    )
    slot, = slots
    baseline = snapshot_path(os.environ["ZB_CHECKPOINT_NAMESPACE_DIR"], args.runtime)
    if baseline.stat().st_size != slot["memory_size"]:
        raise ValueError("snapshot size differs from contiguous packed RAM layout")
    rows = []
    initial_host_frame = None
    try:
        for action in ("state", "read", "write", "sync"):
            guest, transport = guest_request(vsock, action)
            result = {"phase": action, "guest": guest, "transport_ns": transport, "vmm_version": version}
            if action != "state":
                result["freeze"] = {}
                with frozen(api, result["freeze"]):
                    result["host"] = one_page(*api.identity, slot, baseline, guest["guest_pfn"])
                frame = result["host"].pop("host_pfn_private", None)
                if action == "read":
                    initial_host_frame = frame
                result["host"]["same_host_pfn_as_read"] = frame is not None and frame == initial_host_frame
            rows.append(result)
            print(json.dumps(result), flush=True)
        verify(rows)
        print(json.dumps({"phase": "verified", "runtime": args.runtime}), flush=True)
    finally:
        guest_request(vsock, "close")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=("prepare", "measure"))
    parser.add_argument("runtime")
    parser.add_argument("socket")
    run(parser.parse_args())
