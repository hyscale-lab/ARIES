"""Spot-check resident cache by inode for large files in a live private checkpoint and shim."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import time

from backing_residency import cached


def capture(runtime, vm):
    sources = {"checkpoint":runtime/"checkpoints/aries"/vm, "restored_shim":runtime/"shim"/("aries#"+vm)}
    rows={}
    errors=[]
    begin=time.monotonic_ns()
    for kind,root in sources.items():
        if not root.is_dir():
            raise FileNotFoundError(root)
        for directory,_,filenames in os.walk(root,followlinks=False):
            for filename in filenames:
                path=Path(directory)/filename
                try:
                    st=path.lstat()
                    if not stat.S_ISREG(st.st_mode) or st.st_size<64*2**20:
                        continue
                    key=(st.st_dev,st.st_ino)
                    if key not in rows:
                        with path.open("rb",buffering=0) as f:
                            live=os.fstat(f.fileno())
                            if (live.st_dev,live.st_ino,live.st_size)!=(*key,st.st_size):
                                raise ValueError("identity changed before mincore")
                            resident=cached(f.fileno(),st.st_size)
                            end=os.fstat(f.fileno())
                            if live.st_size!=end.st_size or live.st_blocks!=end.st_blocks:
                                raise ValueError("file changed during sample")
                        rows[key]=dict(device=key[0],inode=key[1],logical_bytes=st.st_size,
                                       allocated_disk_bytes=st.st_blocks*512,cached_bytes=resident,paths=[])
                    rows[key]["paths"].append(kind+"/"+str(path.relative_to(root)))
                except (OSError,ValueError) as exc:
                    errors.append(dict(file=str(path.relative_to(root)),error=str(exc)))
    return dict(vm=vm,begin_ns=begin,end_ns=time.monotonic_ns(),files=list(rows.values()),errors=errors,
                total_unique_file_cached_bytes=sum(r["cached_bytes"] for r in rows.values()))


if __name__=="__main__":
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("runtime",type=Path)
    p.add_argument("events",type=Path)
    a=p.parse_args()
    events=[json.loads(l) for l in a.events.read_bytes().splitlines()]
    if events[0]["trial_id"]!="02a77c58-1e9a-4cfb-a332-da5a752e0cfc" or not any(e["event"]=="restore_complete" for e in events) or events[-1]["event"]=="removed":
        raise ValueError("expected a live restored reference Git trial")
    result=capture(a.runtime,events[0]["runtime_id"])
    result["collector_sha256"]=hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    print(json.dumps(result,indent=2))
