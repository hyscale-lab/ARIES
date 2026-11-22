"""Partition smaps charges using verified KVM slots without reading guest bytes."""
from collections import Counter
import os
from pathlib import Path
import re
import struct
import time

HEADER = re.compile(r"^([0-9a-f]+)-([0-9a-f]+) (\S+) ([0-9a-f]+) ([0-9a-f]+):([0-9a-f]+) (\d+)(?:\s+(.*))?$")


def mappings(text):
    row = None
    for line in text.splitlines():
        match = HEADER.match(line)
        if match:
            if row is not None:
                yield row
            lo, hi, mode, offset, major, minor, inode, _ = match.groups()
            row = dict(lo=int(lo,16),hi=int(hi,16),mode=mode,offset=int(offset,16),
                       device=os.makedev(int(major,16),int(minor,16)),inode=int(inode))
        elif row is not None and line.startswith(("Pss:","Rss:","Anonymous:","Private_Dirty:")):
            key,value,unit=line.split()
            if unit!="kB":
                raise ValueError("invalid smaps unit")
            row[key.rstrip(":")]=int(value)*1024
    if row is not None:
        yield row


def covered(lo, hi, slots):
    return sum(max(0,min(hi,s["userspace_addr"]+s["memory_size"])-max(lo,s["userspace_addr"])) for s in slots)


def census(pid, ram, other, snapshot=None):
    begin=time.monotonic_ns()
    proc=Path("/proc")/str(pid)
    snapshot_key=None
    if snapshot is not None:
        st=Path(snapshot).stat()
        snapshot_key=(st.st_dev,st.st_ino)
    totals={}
    page=os.sysconf("SC_PAGE_SIZE")
    snapshot_file_page_count=0
    with (proc/"pagemap").open("rb",buffering=0) as pm:
        for m in mappings((proc/"smaps").read_text()):
            lo,hi=m["lo"],m["hi"]
            mem,device=covered(lo,hi,ram),covered(lo,hi,other)
            if mem==hi-lo:
                role="guest_ram"
            elif device==hi-lo:
                role="device_slot"
            elif mem+device:
                role="mixed_vma"
            else:
                role="outside_kvm"
            if role=="guest_ram" and m["mode"].endswith("p") and snapshot_key==(m["device"],m["inode"]):
                role="guest_ram_snapshot_vma"
                # A private file VMA can contain anonymous CoW pages. Only a present
                # file PTE proves current file backing for an immutable snapshot.
                for start in range(lo,hi,8192*page):
                    length=(min(hi,start+8192*page)-start)//page
                    raw=os.pread(pm.fileno(),length*8,start//page*8)
                    if len(raw)!=length*8:
                        raise OSError("short pagemap read")
                    snapshot_file_page_count+=sum(1 for (e,) in struct.iter_unpack("=Q",raw)
                                                  if e & (1<<63) and e & (1<<61))
            counts=totals.setdefault(role,Counter())
            counts["vmas"]+=1
            for field in ("Rss","Pss","Anonymous","Private_Dirty"):
                counts[field+"_bytes"]+=m[field]
    return dict(begin_ns=begin,end_ns=time.monotonic_ns(),categories=totals,
                current_file_backed_snapshot_rss_bytes=snapshot_file_page_count*page if snapshot_key else None,
                snapshot_inode_verified=bool(snapshot_key))


if __name__=="__main__":
    import json
    print(json.dumps(census(os.getpid(),[],[])))
