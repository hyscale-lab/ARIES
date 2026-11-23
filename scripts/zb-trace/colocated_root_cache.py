"""Sample actual restored immutable-root inode residency without accessing file contents."""
import argparse
import json
import os
from pathlib import Path
import stat
import time

from backing_residency import cached
from vmm_census import mappings


def start_ticks(proc):
    return int((proc / "stat").read_text().rsplit(")", 1)[1].split()[19])


def observe(path, cache):
    receipt = json.loads(path.read_text())
    proc = Path("/proc") / str(receipt["pid"])
    if start_ticks(proc) != receipt["start_ticks"]:
        return None
    root = receipt["root"]
    key = root["device"], root["inode"]
    handles = ([proc/"fd"/str(fd) for fd in root["descriptors"]] if root["evidence"] == "open-fd"
               else [proc/"map_files"/root["mapping"]])
    begin = time.monotonic_ns()
    for descriptor in handles:
        try:
            stream = descriptor.open("rb", buffering=0)
        except FileNotFoundError:
            continue
        with stream:
            st = os.fstat(stream.fileno())
            if (st.st_dev, st.st_ino) != key:
                continue
            if not stat.S_ISREG(st.st_mode) or st.st_size != root["size"]:
                raise ValueError("restored root file size or type changed")
            if key not in cache:
                cache[key] = cached(stream.fileno(), st.st_size)
            cached_bytes = cache[key]
            regions = [m for m in mappings((proc/"smaps").read_text()) if (m["device"], m["inode"]) == key]
            if start_ticks(proc) != receipt["start_ticks"]:
                return None
            return dict(pid=receipt["pid"], start_ticks=receipt["start_ticks"], runtime=receipt["runtime_id"],
                        device=st.st_dev, inode=st.st_ino, size=st.st_size, cached_bytes=cached_bytes,
                        file_rss=sum(m["Rss"]-m["Anonymous"] for m in regions), pss=sum(m["Pss"] for m in regions),
                        begin_ns=begin, end_ns=time.monotonic_ns())
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("studies", type=Path, nargs="+")
    parser.add_argument("--seconds", type=int, required=True)
    parser.add_argument("--interval", type=int, default=15)
    parser.add_argument("--periods", type=int, required=True)
    args = parser.parse_args()
    end = time.monotonic() + args.seconds
    sampled = set()
    while time.monotonic() < end:
        start = time.monotonic()
        for study in args.studies:
            for period in sorted(study.glob("*-?vm")):
                expected = int(period.name.rsplit("-", 1)[-1][:-2])
                paths = sorted(period.glob("*/restored-backing.json"))
                if period in sampled or len(paths) != expected:
                    continue
                sessions = []
                for path in paths:
                    with (path.parent/"events.jsonl").open() as events:
                        for line in events:
                            if not line.endswith("\n"):
                                break
                            event = json.loads(line)
                            if event["event"] == "session_start":
                                sessions.append(event["monotonic_ns"])
                                break
                if len(sessions) != expected or time.monotonic_ns() - max(sessions) < 240_000_000_000:
                    continue
                roles = {}
                cache = {}
                for path in paths:
                    try:
                        result = observe(path, cache)
                    except (FileNotFoundError, ProcessLookupError, json.JSONDecodeError):
                        continue
                    if result:
                        roles[path.parent.name] = result
                if len(roles) == expected:
                    begin = min(r["begin_ns"] for r in roles.values())
                    print(json.dumps(dict(study=study.name, period=period.name, roles=roles,
                                          youngest_session_age_s=(begin-max(sessions))/1e9,
                                          distinct_cached_bytes=sum(cache.values()))), flush=True)
                    sampled.add(period)
        if len(sampled) >= args.periods:
            break
        time.sleep(max(0, args.interval - (time.monotonic()-start)))


if __name__ == "__main__":
    main()
