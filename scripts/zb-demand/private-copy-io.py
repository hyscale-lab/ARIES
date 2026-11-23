"""Calibrate packed private writes and individual 4 KiB reads on a study worker."""
import argparse
import hashlib
import json
import mmap
import multiprocessing
import os
from pathlib import Path
import random
import statistics
import subprocess
import tempfile
import time

PAGE = 4096
CHUNK = 1 << 20


def io_bytes():
    fields = dict(line.split(": ") for line in Path("/proc/self/io").read_text().splitlines())
    return {k: int(fields[k]) for k in ("read_bytes", "write_bytes")}


def timestamp():
    return time.monotonic_ns()


def compete(path, stop, pipe):
    timeline = []
    with open(path, "rb", buffering=0, opener=lambda p, f: os.open(p, f | os.O_DIRECT)) as f, mmap.mmap(-1, CHUNK) as buf:
        size = path.stat().st_size
        offset = 0
        while not stop.is_set():
            begin = timestamp()
            if os.preadv(f.fileno(), [buf], offset) != CHUNK:
                raise ValueError("short background read")
            timeline.append((begin, timestamp()))
            if len(timeline) == 1:
                pipe.send(True)
            offset = (offset + CHUNK) % size
    pipe.send(timeline)


def read_pages(path, offsets, direct, payloads):
    flags = os.O_RDONLY | (os.O_DIRECT if direct else 0)
    fd = os.open(path, flags)
    durations = []
    before = io_bytes()
    try:
        os.posix_fadvise(fd, 0, 0, os.POSIX_FADV_RANDOM)
        with mmap.mmap(-1, PAGE) as buf:
            begin = timestamp()
            for offset in offsets:
                t = timestamp()
                n = os.preadv(fd, [buf], offset)
                durations.append(timestamp() - t)
                chunk_no, within = divmod(offset, CHUNK)
                if n != PAGE or buf[:] != payloads[chunk_no][within:within+PAGE]:
                    raise ValueError("private page readback mismatch")
            end = timestamp()
    finally:
        os.close(fd)
    ordered = sorted(durations)
    return dict(begin_ns=begin, end_ns=end, verified_pages=len(offsets),
                kernel_read_bytes=io_bytes()["read_bytes"]-before["read_bytes"],
                summed_syscall_s=sum(durations)/1e9, median_us=statistics.median(ordered)/1e3,
                p95_us=ordered[(len(ordered)*95+99)//100-1]/1e3)


def measure(folder, payloads, offsets, background):
    stop = multiprocessing.Event()
    pipe, child = multiprocessing.Pipe()
    worker = None
    phases = {}
    if background:
        worker = multiprocessing.Process(target=compete,args=(folder/"competitor",stop,child))
        worker.start()
    try:
        if worker and (not pipe.poll(15) or pipe.recv() is not True):
            raise RuntimeError("background reader did not start")
        path = folder / "saved"
        before = io_bytes()
        with path.open("xb", buffering=0) as f:
            begin = timestamp()
            for block in payloads:
                if f.write(block) != len(block):
                    raise ValueError("short private save")
            submitted = timestamp()
            os.fdatasync(f.fileno())
            end = timestamp()
        phases["save"] = dict(begin_ns=begin, end_ns=end, write_s=(submitted-begin)/1e9,
                              fdatasync_s=(end-submitted)/1e9, saved_bytes=path.stat().st_size,
                              kernel_write_bytes=io_bytes()["write_bytes"]-before["write_bytes"])
        phases["buffered_read"] = read_pages(path, offsets, False, payloads)
        phases["direct_read"] = read_pages(path, offsets, True, payloads)
        if phases["direct_read"]["kernel_read_bytes"] < len(offsets)*PAGE:
            raise ValueError("direct reads did not reach block I/O accounting")
        timeline = []
        if worker:
            stop.set()
            if not pipe.poll(30):
                raise RuntimeError("missing background reader trace")
            timeline = pipe.recv()
        for phase in phases.values():
            phase["concurrent_completed_read_bytes"] = CHUNK * sum(
                phase["begin_ns"] <= t <= phase["end_ns"] for _, t in timeline)
        path.unlink()
        return dict(competing_reader=background, phases=phases)
    finally:
        if worker:
            stop.set()
            worker.join(5)
            if worker.is_alive():
                worker.terminate()
                worker.join()
        pipe.close()
        child.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("parent",type=Path)
    parser.add_argument("--save-mib",type=int,default=180)
    parser.add_argument("--read-mib",type=int,default=84)
    args = parser.parse_args()
    if not 0 < args.read_mib <= args.save_mib:
        raise ValueError("read size must fit the saved file")
    rng = random.Random(204107)
    payloads = [rng.randbytes(CHUNK) for _ in range(args.save_mib)]
    offsets = [i*PAGE for i in rng.sample(range(args.save_mib*CHUNK//PAGE), args.read_mib*CHUNK//PAGE)]
    with tempfile.TemporaryDirectory(prefix="zb-private-copy-",dir=args.parent) as temporary:
        folder = Path(temporary)
        with (folder/"competitor").open("xb",buffering=0) as f:
            for block in payloads:
                if f.write(block) != CHUNK:
                    raise ValueError("short background setup write")
            os.fdatasync(f.fileno())
        samples = [measure(folder, payloads, offsets, other) for other in (False,True,True,False)]
        print(json.dumps(dict(worker=os.uname().nodename, kernel=os.uname().release,
                              boot_id=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                              mount=subprocess.check_output(["findmnt","-n","-o","SOURCE,FSTYPE,OPTIONS","-T",str(folder)],text=True).strip(),
                              page_bytes=PAGE, chunk_bytes=CHUNK, seed=204107,
                              payload_sha256=hashlib.sha256(b"".join(payloads)).hexdigest(),
                              source_sha256=hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                              samples=samples),indent=2))


if __name__ == "__main__":
    main()
