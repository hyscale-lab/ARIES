"""Sample host pressure, memory and live VMM identity without attaching to a guest."""
import argparse
import json
import os
from pathlib import Path
import time


def sample():
    proc = Path("/proc")
    memory = {}
    for line in (proc/"meminfo").read_text().splitlines():
        key, value = line.split(":",1)
        if key in ("MemAvailable","MemTotal","Cached","Dirty","Writeback","SwapFree"):
            memory[key] = int(value.split()[0])*1024
    pressure = {}
    for resource in ("cpu","io","memory"):
        pressure[resource] = {}
        for line in (proc/"pressure"/resource).read_text().splitlines():
            group, *fields = line.split()
            values = dict(field.split("=") for field in fields)
            pressure[resource][group] = int(values["total"])
    vmms = {}
    for directory in proc.glob("[0-9]*"):
        try:
            if (directory/"comm").read_text().strip() not in ("firecracker",".firecracker-re"):
                continue
            args = (directory/"cmdline").read_bytes().split(b"\0")
            rid = args[args.index(b"--id")+1].decode()
            stat = (directory/"stat").read_text().rsplit(")",1)[1].split()
            vmms[rid] = dict(pid=int(directory.name), start_ticks=int(stat[19]),
                             cpu_ticks=int(stat[11])+int(stat[12]))
        except (FileNotFoundError, ProcessLookupError, ValueError, IndexError):
            continue
    return dict(unix_ns=time.time_ns(),monotonic_ns=time.monotonic_ns(),memory_bytes=memory,
                pressure_total_us=pressure,vmms=vmms)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--count",type=int,required=True)
    args = parser.parse_args()
    start = time.monotonic()
    for index in range(args.count):
        time.sleep(max(0,start+index-time.monotonic()))
        print(json.dumps(dict(host=os.uname().nodename,clock_ticks=os.sysconf("SC_CLK_TCK"),
                              **sample())),flush=True)
