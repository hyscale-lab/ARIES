"""Sample the dedicated guest without executing inside the workload."""
import json
from pathlib import Path
import sys
import time

group = Path(sys.argv[1])
daemon_pids = []
for item in Path("/proc").iterdir():
    if item.name.isdigit():
        try:
            cmd = (item / "cmdline").read_bytes()
        except (FileNotFoundError, ProcessLookupError):
            continue
        if b"crab.daemon" in cmd or b"crab.host_inspector" in cmd or b"/fs_monitor" in cmd:
            daemon_pids.append(item)
while True:
    counters = {}
    for file in ("cgroup.procs", "memory.current", "memory.max", "cpu.max", "memory.stat"):
        try:
            raw = (group / file).read_text().strip()
        except FileNotFoundError:
            raw = None
        if file == "memory.stat" and raw:
            raw = {k: int(v) for k, v in (line.split() for line in raw.splitlines()) if k in ("file", "anon", "kernel")}
        counters[file] = raw
    helpers = {}
    for item in daemon_pids:
        try:
            lines = (item / "smaps_rollup").read_text().splitlines()
            helpers[item.name] = int(next(x.split()[1] for x in lines if x.startswith("Pss:")))
        except (FileNotFoundError, ProcessLookupError, StopIteration):
            continue
    counters["helper_pss_kib"] = helpers
    for row in Path("/proc/spl/kstat/zfs/arcstats").read_text().splitlines():
        fields = row.split()
        if fields and fields[0] == "size":
            counters["guest_zfs_arc_bytes"] = int(fields[-1])
            break
    print(json.dumps(dict(ns=time.monotonic_ns(), **counters)), flush=True)
    time.sleep(.1)
