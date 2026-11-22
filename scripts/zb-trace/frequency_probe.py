"""Measure busy MHz under short, CPU-affined loads using Linux turbostat."""
import argparse
import datetime
import json
import os
import subprocess
import sys

LOAD = """
import os, sys, time
cpus = [int(cpu) for cpu in sys.argv[1:]]
end = time.monotonic() + 4
children = []
for cpu in cpus:
    pid = os.fork()
    if pid == 0:
        os.sched_setaffinity(0, {cpu})
        while time.monotonic() < end:
            pass
        os._exit(0)
    children.append(pid)
for pid in children:
    if os.waitpid(pid, 0)[1] != 0:
        sys.exit(1)
"""


def measure(cpus):
    p = subprocess.run(["sudo", "-n", "turbostat", "--quiet", "--show",
                        "CPU,Busy%,Bzy_MHz,TSC_MHz", "--", sys.executable,
                        "-c", LOAD, *map(str, cpus)], text=True, capture_output=True, check=True)
    lines = (p.stdout + p.stderr).splitlines()
    header = next(i for i, line in enumerate(lines) if "Bzy_MHz" in line and "Busy%" in line)
    keys = lines[header].split()
    rows = [dict(zip(keys, line.split())) for line in lines[header+1:] if len(line.split()) == len(keys)]
    loaded = [r for r in rows if r["CPU"].isdigit() and int(r["CPU"]) in cpus]
    if len(loaded) != len(cpus):
        raise ValueError(f"missing CPU rows in {lines!r}")
    return dict(loaded_cpus=cpus, duration_s=4, counters=loaded)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repetitions", type=int, default=2)
    args = parser.parse_args()
    cpus = sorted(os.sched_getaffinity(0))
    records = []
    for _ in range(args.repetitions):
        for group in [cpus[:1], cpus]:
            records.append(measure(group))
    print(json.dumps(dict(utc=datetime.datetime.now(datetime.timezone.utc).isoformat(),
                          hostname=os.uname().nodename.split(".")[0], measurements=records)))
