#!/usr/bin/env python3
"""Write 1 Hz stock AgentENV VMM measurements as JSONL on the worker.

The replay driver atomically replaces BINDINGS with a JSON object keyed by
"workload" and optionally "recording". Each role has a positive "generation",
an absolute "socket" path (or null), and a boolean "allow_zero". Advance the
generation with a null socket and allow_zero=false before a create/resume can
spawn or adopt a VMM. Publish the socket once positively attributed to that
role and retain it until the next generation. Set allow_zero=true when the
driver rules out replacement until its next lifecycle request. Disable AgentENV
auto-resume for this contract. Missing binding evidence can never imply absence.

Stock server JSON debug logs identify the workload socket directory as work_dir
in "starting fresh sandbox" or fc_cwd in "starting sandbox from snapshot config"
under its create_sandbox/resume_sandbox span with the matching sandbox_id. The
startup-pack recorder has its own directory and must have a separate binding.
Run with full host PID visibility and access to smaps_rollup and snapshot files.

Pass only sandbox-owned roots to --snapshot-root. Cache is a mincore count for
identifiable local snapshot files. Shared links, ublk block-device cache, and
OverlayBD daemon memory are outside this measurement.
"""
import argparse
from dataclasses import dataclass
import json
import os
from pathlib import Path
import socket
import stat
import struct
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from backing_residency import cached


SECOND = 1_000_000_000
TICK_HZ = os.sysconf("SC_CLK_TCK")
ROLES = ("workload", "recording")


@dataclass(frozen=True)
class Binding:
    generation: int
    socket: Path | None
    allow_zero: bool


def read_bindings(path):
    try:
        data = json.loads(path.read_bytes())
        if not isinstance(data, dict) or data.keys() - set(ROLES):
            raise ValueError("expected per-role bindings")
        bindings = {}
        for role, entry in data.items():
            generation, path, allow_zero = (
                entry["generation"], entry["socket"], entry["allow_zero"])
            if (type(generation) is not int or generation < 1 or type(allow_zero) is not bool
                    or (path is not None and (not isinstance(path, str) or not Path(path).is_absolute()))):
                raise ValueError(f"invalid {role} binding")
            bindings[role] = Binding(generation, Path(path) if path is not None else None, allow_zero)
        sockets = [binding.socket for binding in bindings.values() if binding.socket is not None]
        if len(sockets) != len(set(sockets)):
            raise ValueError("workload and recording sockets must differ")
        return bindings, None
    except (OSError, ValueError, KeyError, TypeError) as exc:
        return {}, str(exc)


def bind(path):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as peer:
        peer.settimeout(0.1)
        directory = os.open(path.parent, os.O_DIRECTORY)
        try:
            peer.connect(f"/proc/self/fd/{directory}/{path.name}")
        finally:
            os.close(directory)
        pid, _, _ = struct.unpack("3i", peer.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
    # An opened procfs directory keeps its original task identity through PID reuse.
    procfd = os.open(f"/proc/{pid}", os.O_DIRECTORY)
    try:
        proc = Path("/proc/self/fd") / str(procfd)
        state, start_ticks, _ = process_status(proc)
        args = (proc / "cmdline").read_bytes().split(b"\0")
        if (b"--api-sock", os.fsencode(path)) not in zip(args, args[1:]):
            raise ValueError("peer does not have the bound --api-sock argument")
        if state in "ZXx" or process_status(proc)[1] != start_ticks:
            raise ValueError("peer identity changed during binding")
    except BaseException:
        os.close(procfd)
        raise
    return proc, procfd, pid, start_ticks


def process_status(proc):
    # The /proc stat comm field may contain whitespace or a closing parenthesis.
    fields = (proc / "stat").read_text().rsplit(")", 1)[1].split()
    return fields[0], int(fields[19]), int(fields[11]) + int(fields[12])


def rollup(proc):
    counters = {}
    for line in (proc / "smaps_rollup").read_text().splitlines():
        if line.startswith(("Rss:", "Pss:")):
            key, size, unit = line.split()
            if unit != "kB" or int(size) < 0:
                raise ValueError("invalid smaps_rollup size")
            counters[key] = int(size) * 1024
    return counters["Rss:"], counters["Pss:"]


class VMM:
    def __init__(self):
        self.identity = None
        self.binding = None
        self.generation = 0
        self.previous = None

    def close(self):
        if self.identity is not None:
            os.close(self.identity[1])
            self.identity = None

    def process(self):
        proc, _, _, start_ticks = self.identity
        try:
            state, start, ticks = process_status(proc)
        except (FileNotFoundError, ProcessLookupError):
            return None
        if start != start_ticks:
            raise ValueError("process identity changed")
        return None if state in "ZXx" else ticks

    def sample(self, binding):
        begin = time.monotonic_ns()
        return dict(begin_ns=begin, **self.measure(binding), end_ns=time.monotonic_ns())

    def unavailable(self, reason):
        self.previous = None
        return dict(status="unknown", reason=reason, generation=None, pid=None,
                    start_ticks=None, rss_bytes=None, pss_bytes=None, cpu_ticks=None, cpu_cores=None)

    def measure(self, binding):
        try:
            if binding is not None:
                if binding.generation < self.generation:
                    return self.unavailable("stale generation")
                self.generation = binding.generation
            if binding is not None and self.binding is not None:
                if (binding.generation == self.binding.generation
                        and binding.socket != self.binding.socket):
                    return self.unavailable("socket changed within a bound generation")
            if binding is not None and binding.socket is not None and (
                    self.binding is None or binding.generation > self.binding.generation):
                if self.identity is not None and self.process() is not None:
                    return self.unavailable("new binding while the previous generation is live")
                identity = bind(binding.socket)
                self.close()
                self.identity, self.binding, self.previous = identity, binding, None
            if self.identity is None:
                return self.unavailable("no process bound")

            proc, _, pid, start_ticks = self.identity
            identity = dict(generation=self.binding.generation, pid=pid, start_ticks=start_ticks)
            if self.process() is None:
                if binding is None or binding.generation != self.binding.generation or not binding.allow_zero:
                    return self.unavailable("last bound process exited without confirmed suspension")
                cpu = 0 if self.previous is not None and self.previous[1] is None else None
                self.previous = time.monotonic_ns(), None
                return dict(status="exited", **identity, rss_bytes=0, pss_bytes=0, cpu_ticks=None, cpu_cores=cpu)

            rss, pss = rollup(proc)
            cpu_ticks = self.process()
            if cpu_ticks is None:
                raise ValueError("process exited while reading counters")
            now = time.monotonic_ns()
            cpu = None
            if self.previous is not None:
                last_ns, last_ticks = self.previous
                if last_ticks is not None and cpu_ticks >= last_ticks and now > last_ns:
                    cpu = (cpu_ticks - last_ticks) * SECOND / (TICK_HZ * (now - last_ns))
            self.previous = now, cpu_ticks
            return dict(status="live", **identity, rss_bytes=rss, pss_bytes=pss, cpu_ticks=cpu_ticks, cpu_cores=cpu)
        except (OSError, ValueError, KeyError, IndexError) as exc:
            return self.unavailable(str(exc))


def snapshot_cache(roots, checkpoint_paths=None):
    files = {}
    issues = []
    if checkpoint_paths is not None:
        try:
            extra = json.loads(checkpoint_paths.read_bytes())
            if not isinstance(extra, list) or not all(isinstance(p, str) and Path(p).is_absolute() for p in extra):
                raise ValueError("invalid checkpoint cache path list")
            roots = [*roots, *map(Path, extra)]
        except FileNotFoundError:
            pass  # First child snapshot has not been published.
        except (ValueError, OSError) as exc:
            issues.append(str(exc))
    for root in roots:
        entries = ([(root.parent, (), [root.name])] if root.name.endswith(".commit") else
                   os.walk(root, onerror=lambda exc: issues.append(str(exc))))
        for directory, _, names in entries:
            for name in sorted(names):
                if not (name in ("vm_state.bin", "rootfs.ext4", "firecracker-manifest.json", "memory-startup.pack") or name.endswith(".commit")):
                    continue
                path = Path(directory) / name
                try:
                    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                    try:
                        before = os.fstat(fd)
                        if not stat.S_ISREG(before.st_mode) or before.st_nlink != 1:
                            raise ValueError("shared or non-regular snapshot file")
                        if os.geteuid() not in (0, before.st_uid):
                            raise ValueError("mincore requires file ownership or privilege")
                        key = before.st_dev, before.st_ino
                        if key in files:
                            continue
                        size = before.st_size
                        resident = cached(fd, size) if size else 0
                        after = path.stat(follow_symlinks=False)
                        if (after.st_dev, after.st_ino, after.st_ctime_ns) != (*key, before.st_ctime_ns):
                            raise ValueError("snapshot file changed during sampling")
                        files[key] = dict(path=str(path), device=key[0], inode=key[1],
                                          logical_bytes=size, cached_bytes=resident)
                    finally:
                        os.close(fd)
                except (OSError, ValueError) as exc:
                    issues.append(f"{path}: {exc}")
    return dict(status="unconfigured" if not roots else "partial" if issues else "observed",
                observed_cached_bytes=sum(row["cached_bytes"] for row in files.values())
                if files or (roots and not issues) else None, files=list(files.values()), issues=issues)


def sample(sandbox_id, bindings_path, roots, count, output):
    vmms = {role: VMM() for role in ROLES}
    deadline = time.monotonic_ns()
    missed = 0
    index = 0
    try:
        while count is None or index < count:
            time.sleep(max(0, deadline - time.monotonic_ns()) / SECOND)
            wall_ns = time.time_ns()
            bindings, error = read_bindings(bindings_path)
            rows = {role: vmm.sample(bindings.get(role)) for role, vmm in vmms.items()}
            if any(row["status"] == "exited" for row in rows.values()):
                confirmed, _ = read_bindings(bindings_path)
                for role, row in rows.items():
                    if row["status"] == "exited" and confirmed.get(role) != bindings[role]:
                        vmm = vmms[role]
                        if role in confirmed:
                            vmm.generation = max(vmm.generation, confirmed[role].generation)
                        row.update(vmm.unavailable("binding changed during exit observation"))
            cache_begin_ns = time.monotonic_ns()
            cache = snapshot_cache(roots, bindings_path.with_name("checkpoint-cache-paths.json"))
            cache_end_ns = time.monotonic_ns()
            print(json.dumps(dict(schema=1, sandbox_id=sandbox_id, scheduled_ns=deadline,
                                  wall_ns=wall_ns, missed_intervals_before=missed, ticks_per_second=TICK_HZ,
                                  **rows["workload"], recording_vmm=rows["recording"], binding_error=error,
                                  snapshot_cache=dict(begin_ns=cache_begin_ns, end_ns=cache_end_ns, **cache))),
                  file=output, flush=True)
            index += 1
            missed = (time.monotonic_ns() - deadline) // SECOND
            deadline += (missed + 1) * SECOND
    finally:
        for vmm in vmms.values():
            vmm.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("sandbox_id")
    parser.add_argument("bindings", type=Path)
    parser.add_argument("--snapshot-root", action="append", type=Path, default=[],
                        help="sandbox-specific snapshot root, repeatable for distinct backing roots")
    parser.add_argument("--count", type=int)
    args = parser.parse_args()
    if args.count is not None and args.count < 1:
        parser.error("--count must be positive")
    sample(args.sandbox_id, args.bindings, args.snapshot_root, args.count, sys.stdout)
