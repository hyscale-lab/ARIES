#!/usr/bin/env python3
"""Measure stock Firecracker idle-page windows at replay boundaries."""
import argparse
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import struct
import sys
import time

from idle import IdleObserver, start_identity
from vmm_census import census


PHASES = ("session-start", "pre-tool", "post-tool", "session-end")
MIB = 1 << 20
# Linux PROC_PID_INIT_INO. bpf_get_current_pid_tgid reports initial-namespace IDs.
INITIAL_PID_NAMESPACE = 0xEFFFFFFC


class Firecracker:
    def __init__(self, path):
        self.path = Path(path.removeprefix("unix://"))
        self.identity = None
        self.vm_id = None

    def request(self, method, endpoint, state=None):
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
            sock.settimeout(10)
            # Linux sockaddr_un can be shorter than a jailed socket's path.
            dirfd = os.open(self.path.parent, os.O_DIRECTORY)
            try:
                sock.connect(f"/proc/self/fd/{dirfd}/{self.path.name}")
            finally:
                os.close(dirfd)
            pid, _, _ = struct.unpack("3i", sock.getsockopt(
                socket.SOL_SOCKET, socket.SO_PEERCRED, struct.calcsize("3i"),
            ))
            found = pid, start_identity(pid)
            if self.identity is not None and found != self.identity:
                raise RuntimeError("Firecracker API peer changed")
            self.identity = found
            conn = http.client.HTTPConnection("localhost")
            conn.sock = sock
            try:
                body = None if state is None else json.dumps({"state": state})
                headers = {"Content-Type": "application/json"} if body else {}
                conn.request(method, endpoint, body, headers)
                response = conn.getresponse()
                payload = response.read()
                expected = 200 if method == "GET" else 204
                if response.status != expected:
                    raise RuntimeError(f"Firecracker {method} {endpoint}: HTTP "
                                       f"{response.status}: {payload.decode(errors='replace')}")
                return json.loads(payload) if payload else None
            finally:
                conn.close()

    def inspect(self):
        found = self.request("GET", "/")
        self.vm_id = found["id"]
        if found["state"] != "Running":
            raise RuntimeError(f"expected running Firecracker, got {found['state']}")
        if not found["vmm_version"].startswith("1.18."):
            raise RuntimeError(f"unsupported Firecracker version {found['vmm_version']}")
        return found


def ram_spans(memory_bytes):
    # Firecracker v1.18's x86_64 layout skips its two reserved MMIO regions.
    if os.uname().machine != "x86_64" or type(memory_bytes) is not int or memory_bytes <= 0:
        raise RuntimeError("unsupported Firecracker RAM layout")
    spans = []
    for start, length in ((0, 3 << 30), (4 << 30, 252 << 30), (512 << 30, memory_bytes)):
        if memory_bytes == 0:
            break
        length = min(length, memory_bytes)
        spans.append((start, start + length))
        memory_bytes -= length
    return spans


def select_slots(data, pid, start_ticks, tick_ns, mem_bytes):
    # Both /proc/PID/stat start ticks and bpftrace nsecs use the boot clock.
    # Hook freeze and capture timing uses a separate monotonic clock.
    start_ns = int(start_ticks) * tick_ns
    ready = None
    registration = {}
    for line in data.splitlines():
        label, *values = line.split() if line else ("",)
        if label == "ready":
            if ready is not None:
                raise RuntimeError("multiple KVM capture starts in one slot log")
            ready, = map(int, values)
        elif label == "end":
            raise RuntimeError("KVM slot capture has ended")
        elif label == "slot":
            entry, ts, owner, fd, index, flags, gpa, size, hva, result = map(int, values)
            if owner != pid or entry < start_ns or result != 0:
                continue
            key = (fd, index)
            if size == 0:
                registration.pop(key, None)
                continue
            registration[key] = dict(kvm_vm_fd=fd, slot=index, flags=flags, guest_phys_addr=gpa,
                                     memory_size=size, userspace_addr=hva,
                                     registration_entry_boottime_ns=entry,
                                     registration_exit_boottime_ns=ts)
        elif line.strip():
            raise RuntimeError(f"invalid KVM capture record: {line[:100]}")
    if ready is None or ready > start_ns:
        raise RuntimeError("KVM slot tracing was not ready before the VMM started")
    if len({fd for fd, _ in registration}) != 1:
        raise RuntimeError("expected exactly one KVM VM FD for Firecracker")
    slots = sorted(registration.values(), key=lambda s: s["guest_phys_addr"])
    spans = ram_spans(mem_bytes)
    dram, other = [], []
    for slot in slots:
        first = slot["guest_phys_addr"]
        last = first + slot["memory_size"]
        if any(lo <= first < last <= hi for lo, hi in spans):
            if slot["slot"] >> 16 or slot["flags"] & ~1:
                raise RuntimeError("unsupported KVM guest RAM slot flags or address space")
            dram.append(slot)
        else:
            if any(first < hi and lo < last for lo, hi in spans):
                raise RuntimeError("non-RAM KVM slot overlaps expected guest RAM address space")
            other.append(slot)
    # A shorter capture cannot look valid by reporting only the observed slots.
    for lo, hi in spans:
        cursor = lo
        for slot in dram:
            first = slot["guest_phys_addr"]
            if lo <= first < hi:
                if first != cursor:
                    raise RuntimeError("gap or overlap in captured guest RAM slots")
                cursor += slot["memory_size"]
        if cursor != hi:
            raise RuntimeError("incomplete KVM guest RAM capture")
    return dram, other


def frozen_capture(api, observer, phase, step, event):
    freeze = event["freeze"] = {"pause_request_ns": time.monotonic_ns()}
    try:
        api.request("PATCH", "/vm", "Paused")
        freeze["pause_ack_ns"] = time.monotonic_ns()
        if phase != "session-start":
            event["window"] = observer.collect()
        if os.environ.get("ZB_VMM_CENSUS") == "1":
            snapshot = None
            if root := os.environ.get("ZB_CHECKPOINT_NAMESPACE_DIR"):
                snapshot = Path(root) / event["runtime"] / "memory"
            event["vmm_census"] = census(api.identity[0], event["ram_slots"], event["other_kvm_slots"], snapshot)
        if phase != "session-end":
            event["arm"] = observer.arm(step if phase == "pre-tool" else None)
    finally:
        freeze["resume_request_ns"] = time.monotonic_ns()
        try:
            # A lost pause acknowledgement may still leave the guest paused.
            api.request("PATCH", "/vm", "Resumed")
            freeze["resume_ack_ns"] = time.monotonic_ns()
        except Exception as exc:
            freeze["resume_error"] = str(exc)
            # State verification distinguishes a lost acknowledgement from a stuck guest.
            if api.request("GET", "/")["state"] != "Running":
                raise
            freeze["running_confirmed_ns"] = time.monotonic_ns()


def take_boundary(phase, step, api, observer, previous, event):
    if previous is not None:
        prior_phase, prior_step = previous["phase"], previous["step"]
        expected = {
            "pre-tool": ("session-start", "post-tool"),
            "post-tool": ("pre-tool",),
            "session-end": ("session-start", "post-tool", "pre-tool"),
        }
        if (prior_phase not in expected[phase]
                or (phase == "post-tool" and step != prior_step)
                or (phase == "pre-tool" and step <= prior_step)):
            raise RuntimeError(f"unexpected trace boundary {prior_phase}/{prior_step} → {phase}/{step}")
        observer.armed = previous["armed"]
        before = observer.armed["before"]
        observer.armed["before"] = {int(k): v for k, v in before.items()}
        event["window_kind"] = ("tool" if prior_phase == "pre-tool" else
                                "wait" if phase == "pre-tool" else "final")
        if prior_phase == "pre-tool" and phase == "session-end":
            event["window_kind"] = "incomplete-tool"
        event["from_step"] = prior_step
    frozen_capture(api, observer, phase, step, event)


def run(args):
    out = Path(os.environ["ZB_TRACE_OUT"])
    out.parent.mkdir(parents=True, exist_ok=True)
    token = hashlib.sha256(args.runtime.encode()).hexdigest()[:24]
    state_file = out.with_name(f".{out.name}.{token}.armed")
    lock_file = state_file.with_suffix(".lock")
    with lock_file.open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        event = {"schema": "aries.zb.idle-boundary.v1",
                 "clock": "host-CLOCK_MONOTONIC", "runtime": args.runtime,
                 "phase": args.phase, "step": args.step, "hook_begin_ns": time.monotonic_ns()}
        observer = None
        try:
            if Path("/proc/self/ns/pid").stat().st_ino != INITIAL_PID_NAMESPACE:
                raise RuntimeError("KVM slot tracing requires the host's initial PID namespace")
            api = Firecracker(args.socket)
            info = api.inspect()
            if args.phase == "session-start":
                if state_file.exists():
                    raise RuntimeError("runtime already has an armed idle window")
                previous = None
            else:
                previous = json.loads(state_file.read_text())
                if list(api.identity) != previous["identity"] or api.vm_id != previous["vm_id"]:
                    raise RuntimeError("armed window belongs to a different VMM")
            machine = api.request("GET", "/machine-config")
            mem_bytes = machine["mem_size_mib"] * MIB
            tick_ns = 10**9 // os.sysconf("SC_CLK_TCK")
            slots, device_slots = select_slots(Path(os.environ["ZB_SLOT_EVENTS"]).read_text(),
                                               *api.identity, tick_ns, mem_bytes)
            if previous and previous["slots"] != slots:
                raise RuntimeError("guest RAM slot registration changed after arm")
            event.update(vmm_pid=api.identity[0], vmm_start_ticks=api.identity[1],
                         vm_id=api.vm_id, vmm_version=info["vmm_version"],
                         guest_ram_bytes=mem_bytes, ram_slots=slots, other_kvm_slots=device_slots)
            observer = IdleObserver(api.identity[0], api.vm_id, slots)
            if observer.start != api.identity[1]:
                raise RuntimeError("VMM identity changed before opening pagemap")
            take_boundary(args.phase, args.step, api, observer, previous, event)
            if observer.armed is None:
                state_file.unlink()
            else:
                next_state = dict(identity=api.identity, vm_id=api.vm_id, phase=args.phase,
                                  step=args.step, slots=slots, armed=observer.armed)
                newest = state_file.with_suffix(".next")
                fd = os.open(newest, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
                with os.fdopen(fd, "w") as f:
                    f.write(json.dumps(next_state, separators=(",", ":")))
                os.replace(newest, state_file)
            event["status"] = "ok"
        except BaseException as exc:
            event["status"] = "error"
            event["error"] = str(exc)
            raise
        finally:
            if observer is not None:
                observer.close()
            event["hook_capture_and_state_end_ns"] = time.monotonic_ns()
            with out.open("a") as f:
                f.write(json.dumps(event, separators=(",", ":")) + "\n")


def signal_error(signum, _):
    raise InterruptedError(f"trace hook received signal {signum}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=PHASES)
    parser.add_argument("runtime")
    parser.add_argument("step", type=int)
    parser.add_argument("socket")
    args = parser.parse_args()
    if (args.phase in ("session-start", "session-end") and args.step != 0
            or args.phase in ("pre-tool", "post-tool") and args.step <= 0):
        parser.error("step must be zero for session boundaries and positive for tool boundaries")
    for signum in (signal.SIGHUP, signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, signal_error)
    try:
        run(args)
    except Exception as exc:
        sys.exit(f"idle trace {args.phase}/{args.step}: {exc}")


if __name__ == "__main__":
    main()
