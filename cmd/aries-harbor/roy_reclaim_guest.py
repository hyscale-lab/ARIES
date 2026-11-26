"""Measure the named RDKit ELF files without faulting pages into the guest."""
import ctypes as C
import importlib.util
import json
import os
from pathlib import Path
import sys
import time

from ctypes import CDLL, c_int, c_size_t, c_void_p, c_long, POINTER, c_ubyte

mode, = sys.argv[1:]
assert mode in ("read", "control", "reclaim", "capabilities")
if mode == "capabilities":
    cgroup = Path("/sys/fs/cgroup/memory.reclaim")
    mount = [s for s in Path("/proc/self/mountinfo").read_text().splitlines()
             if " - cgroup2 " in s]
    print(json.dumps({
        "kernel": os.uname().release,
        "cgroup2": mount,
        "memory_reclaim_present": cgroup.is_file(),
        "memory_reclaim_writable": os.access(cgroup, os.W_OK),
        "fadvise_dontneed": os.POSIX_FADV_DONTNEED,
        "mincore": hasattr(CDLL(None), "mincore"),
        "page_reporting_order": Path("/sys/module/page_reporting/parameters/page_reporting_order").read_text().strip(),
        "damon_enabled": Path("/sys/module/damon_reclaim/parameters/enabled").read_text().strip(),
    }))
    raise SystemExit

page = os.sysconf("SC_PAGE_SIZE")
libc = CDLL(None, use_errno=True)
libc.mmap.argtypes = (c_void_p, c_size_t, c_int, c_int, c_int, c_long)
libc.mmap.restype = c_void_p
libc.mincore.argtypes = (c_void_p, c_size_t, POINTER(c_ubyte))
libc.munmap.argtypes = (c_void_p, c_size_t)

package = Path(importlib.util.find_spec("rdkit").origin).parent
libs = sorted({p.resolve(strict=True) for p in
               [package / "rdBase.so", package / "Chem/rdchem.so",
                *(package.parent / "rdkit.libs").glob("libRDKit*.so*")]})
assert libs and len(libs) <= 128


def observe():
    sample = {}
    for path in libs:
        with path.open("rb") as stream:
            size = os.fstat(stream.fileno()).st_size
            page_count = size // page
            if not page_count:
                continue
            # Complete pages only. Linux fadvise may keep partial last pages.
            length = page_count * page
            addr = libc.mmap(None, length, 0, 2, stream.fileno(), 0)
            if addr == c_void_p(-1).value:
                raise OSError(C.get_errno(), "mmap")
            try:
                vec = (c_ubyte * page_count)()
                if libc.mincore(addr, length, vec):
                    raise OSError(C.get_errno(), "mincore")
                sample[str(path)] = [i for i, resident in enumerate(vec) if resident & 1]
            finally:
                if libc.munmap(addr, length):
                    raise OSError(C.get_errno(), "munmap")
    keys = {"workingset_refault_file", "pgmajfault", "pgsteal_direct"}
    counters = {k: int(v) for k, v in
                (s.split() for s in Path("/proc/vmstat").read_text().splitlines())
                if k in keys}
    return {"files": sample, "vmstat": counters, "guest_ns": time.monotonic_ns()}


before = observe()
if mode == "read":
    print(json.dumps({"page_bytes": page, "before": before}))
    raise SystemExit

started = time.monotonic_ns()
for path, resident in before["files"].items():
    if not resident:
        continue
    with open(path, "rb") as stream:
        if mode == "reclaim":
            os.posix_fadvise(stream.fileno(), 0, 0, os.POSIX_FADV_DONTNEED)
ended = time.monotonic_ns()
print(json.dumps({"page_bytes": page, "before": before, "after": observe(),
                  "action_begin_guest_ns": started, "action_end_guest_ns": ended}))
