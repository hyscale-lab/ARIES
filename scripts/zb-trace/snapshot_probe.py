"""Diagnostic page content equality for a single verified zero-based guest RAM slot."""
import hashlib
import os
from pathlib import Path
import random
import struct

PAGE = 4096
PRESENT = 1 << 63
PFN_MASK = (1 << 55)-1


def selected(slot, count):
    if slot["guest_phys_addr"] != 0 or slot["memory_size"]%PAGE or os.sysconf("SC_PAGE_SIZE")!=PAGE:
        raise ValueError("sample requires one zero-based slot and 4 KiB pages")
    pages=slot["memory_size"]//PAGE
    return sorted(random.Random(693587).sample(range(pages), min(pages,count)))


def sample_live(pid, slots, count):
    if len(slots)!=1 or not 1 <= count <= 8192:
        raise ValueError("unsupported sample bounds")
    slot, = slots
    root=Path("/proc")/str(pid)
    rows=[]
    with (root/"mem").open("rb",buffering=0) as mem, (root/"pagemap").open("rb",buffering=0) as pm:
        for gfn in selected(slot,count):
            addr=slot["userspace_addr"]+PAGE*gfn
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
    if len(slots)!=1 or Path(path).stat().st_size!=slots[0]["memory_size"]:
        raise ValueError("snapshot file does not match single packed memory slot")
    with open(path,"rb",buffering=0) as f:
        rows=[]
        for gfn in selected(slots[0],count):
            data=os.pread(f.fileno(),PAGE,PAGE*gfn)
            if len(data)!=PAGE:
                raise OSError("short snapshot page")
            rows.append(dict(gfn=gfn,digest=hashlib.sha256(data).hexdigest()))
    return rows
