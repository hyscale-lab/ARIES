"""Export stock numeric observations, using a session-relative monotonic clock."""
import argparse
import csv
import gzip
import json
from pathlib import Path


def read(path):
    if not path.exists():
        path = Path(str(path) + ".gz")
    if path.exists():
        opener = gzip.open if path.suffix == ".gz" else open
        with opener(path, "rt") as f:
            yield from map(json.loads, f)


def counters(run, start, events):
    for host in read(run / "host.jsonl"):
        row = {"stream":"host","t_ns":host["begin_ns"]-start}
        for name, item in [("vmm",host),("recording_vmm",host.get("recording_vmm",{}))]:
            row[name] = {k:item.get(k) for k in ("generation","rss_bytes","pss_bytes","cpu_ticks","cpu_cores","status")}
        row["snapshot_cached_bytes"] = host.get("snapshot_cache",{}).get("observed_cached_bytes")
        row["missed_ticks"] = host.get("missed_intervals_before")
        yield row
    for guest in read(run / "guest.jsonl"):
        t = guest.get("host_receive_ns", guest.get("scheduled_ns"))
        row = {"stream":"guest","t_ns":None if t is None else t-start,"guest_begin_ns":guest.get("guest_begin_ns"),"missed_ticks":guest.get("missed_ticks"),"values":{}}
        for k,data in guest.get("source",{}).items():
            if k in ("memory.current","memory.stat","memory.events","cpu.stat","/proc/meminfo","/proc/vmstat"):
                if k=="memory.current":
                    row["values"][k]=int(data)
                else:
                    row["values"][k]={a.rstrip(":"):int(b) for a,b,*_ in (l.split() for l in data.splitlines())}
            if k in ("io.pressure","memory.pressure","io.stat","/proc/buddyinfo"):
                # Kernel counter rows have a closed grammar and contain no tool payloads.
                import re
                assert re.fullmatch(r"[A-Za-z0-9_= .,:\-\s]+",data)
                row["values"][k]=data.splitlines()
        yield row
    for ev in events:
        if "monotonic_ns" in ev:
            row={"stream":"event","t_ns":ev["monotonic_ns"]-start,"event":ev["event"]}
            row.update({k:ev[k] for k in ("sequence","exit_code","expected_exit_code") if isinstance(ev.get(k),int)})
            yield row


if __name__=="__main__":
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("results",type=Path)
    p.add_argument("target",type=Path)
    args=p.parse_args()
    with (args.results/"summary.csv").open() as f:
        sessions=[r for r in csv.DictReader(f) if r["phase"]=="Session"]
    for r in sessions:
        run=args.results/r["recording"]
        events=list(read(run/"events.jsonl")) or list(read(run/"replay.jsonl"))
        start=next(e["monotonic_ns"] for e in events if e["event"]=="session_start")
        path=args.target/r["recording"]/"counters.jsonl.gz"
        path.parent.mkdir(parents=True,exist_ok=True)
        with path.open("wb") as raw,gzip.GzipFile(filename="",fileobj=raw,mode="wb",mtime=0) as out:
            for row in counters(run,start,events):
                out.write((json.dumps(row,separators=(",",":"))+"\n").encode())
