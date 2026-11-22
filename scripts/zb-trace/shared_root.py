"""Observe common resident immutable-root frames across two restored VMMs."""
import argparse
import json
import os
from pathlib import Path
import struct
import time

from backing_residency import cached
from sample_vmm import process_times
from vmm_census import mappings


def observe(samples, name):
    if not samples.exists() or not samples.stat().st_size:
        return None
    with samples.open() as stream:
        identity = json.loads(stream.readline())
    proc = Path("/proc") / str(identity["pid"])
    try:
        ticks, _ = process_times(proc)
    except FileNotFoundError:
        return None
    if ticks != identity["start_ticks"]:
        return None
    frames = {}
    total_pss = 0
    file_size = None
    file_id = None
    cache = None
    with (proc / "pagemap").open("rb", buffering=0) as pagemap:
        for m in mappings((proc / "smaps").read_text()):
            if not m["inode"]:
                continue
            link = proc / "map_files" / f'{m["lo"]:x}-{m["hi"]:x}'
            if Path(os.readlink(link)).name != name:
                continue
            st = link.stat()
            key = (st.st_dev, st.st_ino)
            if file_id is not None and file_id != key:
                raise ValueError("immutable-root mappings reference different inodes")
            if file_id is None:
                with link.open("rb", buffering=0) as f:
                    cache = cached(f.fileno(), st.st_size)
            file_id, file_size = key, st.st_size
            total_pss += m["Pss"]
            data = os.pread(pagemap.fileno(), (m["hi"]-m["lo"])//4096*8, m["lo"]//4096*8)
            for i, (entry,) in enumerate(struct.iter_unpack("=Q", data)):
                if entry >> 63 and entry & (1 << 61):
                    pfn = entry & ((1 << 55)-1)
                    if not pfn:
                        raise ValueError("physical frame numbers unavailable")
                    frames[m["offset"]//4096+i] = pfn
    if file_id is None or process_times(proc)[0] != ticks:
        return None
    return dict(pid=identity["pid"], ticks=ticks, file_id=file_id, file_size=file_size,
                cached_bytes=cache, root_pss=total_pss, mapped_file_pages=len(frames)), frames


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("root_image", help="verified immutable-root basename")
    args = parser.parse_args()
    if os.sysconf("SC_PAGE_SIZE") != 4096:
        raise ValueError("requires 4 KiB host pages")
    deadline = time.monotonic() + 420
    while time.monotonic() < deadline:
        begin = time.monotonic_ns()
        snapshots = {}
        errors = {}
        for role in ("a", "b"):
            try:
                result = observe(args.directory / role / "samples.jsonl", args.root_image)
                if result:
                    snapshots[role] = result
            except (FileNotFoundError, ProcessLookupError) as exc:
                errors[role] = type(exc).__name__
        row = dict(begin_ns=begin, end_ns=time.monotonic_ns(), errors=errors,
                   vms={k:s[0] for k,s in snapshots.items()})
        if len(snapshots) == 2:
            (a, x), (b, y) = snapshots.values()
            row["same_inode"] = a["file_id"] == b["file_id"]
            both = x.keys() & y.keys()
            row["same_offset_pages"] = len(both)
            row["same_offset_same_frame_pages"] = sum(x[i] == y[i] for i in both)
            row["distinct_physical_frames"] = len(set(x.values()) | set(y.values()))
        print(json.dumps(row), flush=True)
        last = args.directory / "b/events.jsonl"
        if last.exists() and '"event":"removed"' in last.read_text():
            break
        time.sleep(3)


if __name__ == "__main__":
    main()
