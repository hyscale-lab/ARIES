#!/usr/bin/env python3
"""Bounded host-idle feasibility observer. Requires captured KVM slot metadata.

Reports only stable, resident, exclusive, LRU base pages. Compound pages and
mapping changes are explicit unknowns, never fabricated per-4KiB observations.
This is a calibration utility, not an admitted whole-guest observer.
"""
import argparse
import json
import os
import struct
import time

PAGE = 4096
U64 = struct.Struct("=Q")
PRESENT = 1 << 63
PFNMASK = (1 << 55) - 1
LRU = 1 << 5
COMPOUND = (1 << 15) | (1 << 16) | (1 << 17) | (1 << 22)
KSM = 1 << 21
ZERO = 1 << 24


def word(fd, index):
    data = os.pread(fd, 8, index * 8)
    if len(data) != 8:
        raise OSError("short kernel metadata read")
    return U64.unpack(data)[0]


def start_identity(pid):
    with open(f"/proc/{pid}/stat") as f:
        return f.read().rsplit(")", 1)[1].split()[19]


class IdleObserver:
    def __init__(self, pid, vm_id, slots):
        self.pid = pid
        self.vm_id = vm_id
        self.start = start_identity(pid)
        self.slots = slots
        for s in slots:
            if any(s[k] % PAGE for k in ("guest_phys_addr", "userspace_addr", "memory_size")):
                raise ValueError("unaligned KVM slot")
        self.fds = []
        try:
            self.pm = self._open(f"/proc/{pid}/pagemap", os.O_RDONLY)
            self.flags = self._open("/proc/kpageflags", os.O_RDONLY)
            self.count = self._open("/proc/kpagecount", os.O_RDONLY)
            self.idle = self._open("/sys/kernel/mm/page_idle/bitmap", os.O_RDWR)
        except BaseException:
            self.close()
            raise
        self.armed = None

    def _open(self, path, mode):
        fd = os.open(path, mode)
        self.fds.append(fd)
        return fd

    def close(self):
        for fd in self.fds:
            os.close(fd)
        self.fds = []

    def hva(self, gpa):
        matches = [s for s in self.slots if s["guest_phys_addr"] <= gpa < s["guest_phys_addr"] + s["memory_size"]]
        if len(matches) != 1:
            raise ValueError("GPA not in exactly one captured KVM memory slot")
        s = matches[0]
        return s["userspace_addr"] + gpa - s["guest_phys_addr"]

    def mapping(self, gpas):
        if start_identity(self.pid) != self.start:
            raise RuntimeError("VMM process identity changed")
        out = {}
        for gpa in gpas:
            if gpa % PAGE:
                raise ValueError("unaligned GPA")
            e = word(self.pm, self.hva(gpa) // PAGE)
            reason = None
            flags = None
            pfn = e & PFNMASK
            if not e & PRESENT:
                reason = "not-resident"
            elif not pfn:
                reason = "pfn-masked-or-zero"
            else:
                flags = word(self.flags, pfn)
                if flags & COMPOUND:
                    reason = "compound-resolution-unsupported"
                elif flags & (KSM | ZERO):
                    reason = "shared-or-zero-page"
                elif not flags & LRU:
                    reason = "not-on-lru"
                elif word(self.count, pfn) != 1:
                    reason = "nonexclusive-or-transition"
            out[gpa] = {"pfn": pfn, "unknown": reason, "kpageflags": flags}
        return out

    def resident_inventory(self):
        """Enumerate all slot residency from pagemap without reading guest contents."""
        begin = time.monotonic_ns()
        resident, absent = [], 0
        for slot in self.slots:
            size = slot["memory_size"] // PAGE
            data = os.pread(self.pm, size * 8, slot["userspace_addr"] // PAGE * 8)
            if len(data) != size * 8:
                raise OSError("short full-slot pagemap read")
            for i, (entry,) in enumerate(struct.iter_unpack("=Q", data)):
                if entry & PRESENT:
                    resident.append(slot["guest_phys_addr"] + i * PAGE)
                else:
                    absent += 1
        return {"resident_gpas": resident, "nonresident_pages": absent,
                "scan_begin_ns": begin, "scan_end_ns": time.monotonic_ns()}

    def _bits(self, pfns):
        out = {}
        for pfn in pfns:
            block = pfn // 64
            out[block] = out.get(block, 0) | (1 << (pfn % 64))
        return out

    def arm(self, gpas):
        if self.armed is not None:
            raise RuntimeError("one observer timeline only: collect before rearming")
        begin = time.monotonic_ns()
        before = self.mapping(gpas)
        blocks = self._bits(m["pfn"] for m in before.values() if m["unknown"] is None)
        for block, bits in sorted(blocks.items()):
            if os.pwrite(self.idle, U64.pack(bits), block * 8) != 8:
                raise OSError("short idle write")
        self.armed = (begin, time.monotonic_ns(), before)
        return {"arm_begin_ns": begin, "arm_end_ns": self.armed[1]}

    def collect(self):
        if self.armed is None:
            raise RuntimeError("collect without arm")
        arm_begin, arm_end, before = self.armed
        begin = time.monotonic_ns()
        after = self.mapping(before)
        blocks = self._bits(m["pfn"] for m in before.values() if m["unknown"] is None)
        idle_words = {block: word(self.idle, block) for block in sorted(blocks)}
        # Read mapping again after scanning: transitions are not access evidence.
        final = self.mapping(before)
        accessed, idle, unknown, unknown_flags = [], [], {}, {}
        for gpa, prior in before.items():
            reason = prior["unknown"] or after[gpa]["unknown"] or final[gpa]["unknown"]
            if prior["pfn"] != after[gpa]["pfn"] or prior["pfn"] != final[gpa]["pfn"]:
                reason = "host-mapping-changed"
            if reason:
                unknown[str(gpa)] = reason
                unknown_flags[str(gpa)] = [prior.get("kpageflags"), after[gpa].get("kpageflags"), final[gpa].get("kpageflags")]
            elif idle_words[prior["pfn"] // 64] & (1 << (prior["pfn"] % 64)):
                idle.append(gpa)
            else:
                accessed.append(gpa)
        self.armed = None
        return {"schema": "aries.tsb.access-calibration.v1", "vm_instance": self.vm_id,
                "vmm_pid": self.pid, "vmm_start_ticks": self.start,
                "clock": "host-CLOCK_MONOTONIC", "page_bytes": PAGE,
                "arm_begin_ns": arm_begin, "arm_end_ns": arm_end,
                "scan_begin_ns": begin, "scan_end_ns": time.monotonic_ns(),
                "accessed_gpas": accessed, "idle_gpas": idle, "unknown_gpas": unknown,
                "unknown_kpageflags_before_after_final": unknown_flags,
                "coverage_scope": "requested captured-KVM GPAs; stable exclusive host LRU base pages only",
                "caveats": ["KVM clear_young does not flush secondary TLBs",
                            "transient non-LRU/locked-page skips cannot be excluded by endpoint checks",
                            "host/VMM accesses also clear idle; sandbox causal ownership is not proven",
                            "compound granularity unsupported; unknown is not idle",
                            "mapping A-B-A transitions between endpoint checks are unobserved"]}


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--pid", type=int, required=True)
    ap.add_argument("--vm-id", required=True)
    ap.add_argument("--slots", required=True, help="JSON list of exact captured KVM regions")
    ap.add_argument("--gpas", required=True, help="JSON list of guest physical page addresses")
    ap.add_argument("--seconds", type=float, default=1)
    args = ap.parse_args()
    with open(args.slots) as f:
        slots = json.load(f)
    with open(args.gpas) as f:
        gpas = json.load(f)
    observer = IdleObserver(args.pid, args.vm_id, slots)
    try:
        observer.arm(gpas)
        time.sleep(args.seconds)
        print(json.dumps(observer.collect()))
    finally:
        observer.close()

if __name__ == "__main__":
    main()
