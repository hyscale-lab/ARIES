"""Diagnostic page content equality for the verified KVM RAM slots of a restored VM."""
import hashlib
import os
from pathlib import Path
import random
import struct

PAGE = 4096
PRESENT = 1 << 63
PFN_MASK = (1 << 55)-1


def selected(slots, count):
    """Return (guest frame number, host address, packed file offset) for uniformly sampled RAM."""
    if not slots or not 1 <= count <= 8192 or os.sysconf("SC_PAGE_SIZE") != PAGE:
        raise ValueError("unsupported snapshot sample bounds")
    layout = sorted(slots, key=lambda s:s["guest_phys_addr"])
    total = previous_end = 0
    for slot in layout:
        base, size = slot["guest_phys_addr"], slot["memory_size"]
        if base < previous_end or size<=0 or any(v%PAGE for v in (base,size,slot["userspace_addr"])):
            raise ValueError("overlapping or unaligned guest RAM slot")
        previous_end = base + size
        total += size//PAGE
    # Firecracker's file restore packs RAM regions contiguously, in guest address order.
    # The x86 MMIO gap and non-RAM KVM slots do not occupy image bytes.
    indices = sorted(random.Random(693587).sample(range(total),min(total,count)))
    region = start = 0
    rows = []
    for ordinal in indices:
        offset = ordinal*PAGE
        while offset >= start + layout[region]["memory_size"]:
            start += layout[region]["memory_size"]
            region += 1
        slot = layout[region]
        within = offset-start
        rows.append(((slot["guest_phys_addr"]+within)//PAGE,slot["userspace_addr"]+within,offset))
    return rows


def sample_live(pid, slots, count):
    root=Path("/proc")/str(pid)
    rows=[]
    with (root/"mem").open("rb",buffering=0) as mem, (root/"pagemap").open("rb",buffering=0) as pm:
        for gfn,addr,_ in selected(slots,count):
            offset=addr//PAGE*8
            entry,=struct.unpack("=Q",os.pread(pm.fileno(),8,offset))
            row={"gfn":gfn}
            if not entry & PRESENT:
                row["state"]="absent"
            elif not entry & PFN_MASK:
                row["state"]="masked_or_zero"
            else:
                data=os.pread(mem.fileno(),PAGE,addr)
                later,=struct.unpack("=Q",os.pread(pm.fileno(),8,offset))
                if len(data)!=PAGE or (later & (PRESENT|PFN_MASK))!=(entry & (PRESENT|PFN_MASK)):
                    row["state"]="mapping_changed"
                else:
                    row.update(state="resident",digest=hashlib.sha256(data).hexdigest())
            rows.append(row)
    return rows


def sample_snapshot(path, slots, count):
    if Path(path).stat().st_size!=sum(slot["memory_size"] for slot in slots):
        raise ValueError("snapshot file does not match packed RAM size")
    with open(path,"rb",buffering=0) as f:
        rows=[]
        for gfn,_,offset in selected(slots,count):
            data=os.pread(f.fileno(),PAGE,offset)
            if len(data)!=PAGE:
                raise OSError("short snapshot page")
            rows.append(dict(gfn=gfn,digest=hashlib.sha256(data).hexdigest()))
    return rows
