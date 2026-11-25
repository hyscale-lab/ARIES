"""Guest-wide counters read by a persistent workload task on each host tick."""
import os
from pathlib import Path


def selected(path, keys):
    rows = (line.split() for line in Path(path).read_text().splitlines())
    return {row[0].rstrip(":"): int(row[1]) for row in rows if row[0].rstrip(":") in keys}


def guest_counters():
    mem_keys = set("MemTotal MemFree MemAvailable Cached Buffers Shmem AnonPages Slab Dirty Writeback".split())
    vm_keys = set("workingset_refault_file pgmajfault pgscan_kswapd pgscan_direct pgsteal_kswapd pgsteal_direct".split())
    return dict(meminfo_kib=selected("/proc/meminfo", mem_keys),
                vmstat=selected("/proc/vmstat", vm_keys),
                buddyinfo=Path("/proc/buddyinfo").read_text().splitlines())

import json
import sys
import time

def cgroup():
    return next(line for line in Path('/proc/self/cgroup').read_text().splitlines() if line.startswith('0::'))

for line in sys.stdin:
    begin = time.monotonic_ns()
    identity = cgroup()
    if identity not in Path('/proc/1/cgroup').read_text().splitlines():
        raise ValueError('counter process outside workload cgroup')
    row = guest_counters()
    root = Path('/sys/fs/cgroup')
    entries = [root / identity[3:].lstrip('/'), root]
    for cg in entries:
        try:
            if str(os.getpid()) not in (cg / 'cgroup.procs').read_text().splitlines():
                continue
            row['cgroup_memory_stat'] = selected(cg / 'memory.stat', {'workingset_refault_anon', 'workingset_refault_file', 'pgscan', 'pgsteal', 'pgmajfault'})
            break
        except (FileNotFoundError, PermissionError):
            continue
    row.update(tick=int(line), cgroup=identity, guest_read_begin_ns=begin, guest_read_end_ns=time.monotonic_ns())
    print(json.dumps(row), flush=True)
