#!/usr/bin/env python3
"""Filter strace stderr in memory; retain only successful KVM RAM slot metadata.
Attach strace -f -e trace=ioctl -p CONTAINERD_PID -o '|python3 ...capture_slots.py OUTPUT'
before VM creation. Stop/detach strace before measurement. No other ioctl data is
saved. This does not reconstruct slots created before attachment.
"""
import json
import re
import sys
import time

pat = re.compile(r"KVM_SET_USER_MEMORY_REGION, \{slot=(\d+), flags=([^,]+), guest_phys_addr=(0x[0-9a-f]+|\d+), memory_size=(0x[0-9a-f]+|\d+), userspace_addr=(0x[0-9a-f]+|\d+)\}\)\s+= 0")
def parse_slot(line):
    m = pat.search(line)
    if not m:
        return None
    slot, flags, gpa, size, hva = m.groups()
    pid = re.match(r"(?:\[pid\s+)?(\d+)", line)
    return {"schema": "aries.tsb.kvm-slot.v1", "host_monotonic_ns": time.monotonic_ns(),
            "tid": int(pid.group(1)) if pid else None,
            "slot": int(slot), "flags": flags, "guest_phys_addr": int(gpa, 0),
            "memory_size": int(size, 0), "userspace_addr": int(hva, 0)}

if __name__ == "__main__":
    with open(sys.argv[1], "x") as out:
        for line in sys.stdin:
            event = parse_slot(line)
            if event:
                out.write(json.dumps(event) + "\n")
                out.flush()
