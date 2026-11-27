#!/usr/bin/env python3
"""Collect four stock guest-RAM mappings against a common snapshot, without accessing guest bytes."""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
import json
import os
from pathlib import Path
import time

from deep import PAGE, Firecracker, bind, census, dirty_bytes, layout

now = time.monotonic_ns


def load(path):
    return json.loads(Path(path).read_bytes())


def save(path, data):
    tmp = path.with_suffix(path.suffix + '.partial')
    with tmp.open('x') as stream:
        json.dump(data, stream, separators=(',', ':'))
        stream.write('\n')
    if path.exists():
        raise FileExistsError(path)
    tmp.rename(path)


class Guest:
    def __init__(self, stack, sock):
        self.proc, fd, pid, start = bind(Path(sock))
        stack.callback(os.close, fd)
        self.api = Firecracker(str(sock))
        self.api.identity = pid, str(start)
        self.info = self.api.request('GET', '/')
        machine = self.api.request('GET', '/machine-config')
        if (machine.get('track_dirty_pages') is not True or machine['mem_size_mib'] != 4096
                or machine['vcpu_count'] != 2 or os.sysconf('SC_PAGE_SIZE') != PAGE):
            raise ValueError('expected stock dirty-tracked 2 vCPU, 4 GiB RAM and 4 KiB pages')
        self.regions = layout(self.api.request('GET', '/vm/guest-memory-regions'), 4096 << 20)
        self.metadata = dict(socket=str(sock), pid=pid, start_ticks=start, info=self.info, machine=machine,
                             regions=self.regions, monotonic_ns=now())
        self.slots = [dict(userspace_addr=r['base_host_virt_addr'], memory_size=r['size']) for r in self.regions]

    def state(self, expected):
        if self.api.request('GET', '/')['state'] != expected:
            raise RuntimeError(f"expected {self.metadata['socket']} in state {expected}")

    def dirty(self):
        return self.api.request('GET', '/vm/dirty-memory-ranges')

    def pages(self):
        import numpy as np
        arrays = []
        with (self.proc / 'pagemap').open('rb', buffering=0) as pm:
            for r in self.regions:
                n = r['size'] // PAGE
                raw = os.pread(pm.fileno(), n * 8, r['base_host_virt_addr'] // PAGE * 8)
                if len(raw) != n * 8:
                    raise OSError('short pagemap read')
                arrays.append(np.frombuffer(raw, dtype='=u8'))
        return np.concatenate(arrays)

    def capture(self):
        self.state('Paused')
        begin = now()
        charge = census(self.metadata['pid'], self.slots, [])['categories']
        if 'mixed_vma' in charge or 'guest_ram' not in charge:
            raise ValueError('could not isolate RAM VMAs in smaps')
        raw = self.pages()
        dirty = self.dirty()
        return raw, dirty, dict(begin_ns=begin, end_ns=now(), **charge['guest_ram'])


def setup(metadata, dirty):
    regions = layout(metadata['regions'], metadata['machine']['mem_size_mib'] << 20)
    return dict(**metadata, setup_dirty_bytes=dirty_bytes(dirty, regions), dirty=dirty)


def signature(meta):
    spans = [(r['offset'], r['guest_phys_addr'], r['size'], r['page_size'])
             for r in layout(meta['regions'], meta['machine']['mem_size_mib'] << 20)]
    gpa = sorted((s[1], s[1] + s[2]) for s in spans)
    if any(lo < 0 or lo % PAGE for lo, _ in gpa) or any(a[1] > b[0] for a, b in zip(gpa, gpa[1:])):
        raise ValueError('ambiguous GPA/ancestor offset correspondence')
    return spans


def dirty_mask(dirty, meta):
    import numpy as np
    size = meta['machine']['mem_size_mib'] << 20
    dirty_bytes(dirty, layout(meta['regions'], size))
    dirty_map = np.zeros(size // PAGE, dtype=bool)
    for r in dirty['ranges']:
        i = r['image_offset'] // PAGE
        dirty_map[i:i + r['length'] // PAGE] = True
    return dirty_map


def main():
    p = argparse.ArgumentParser(description=__doc__)
    sub = p.add_subparsers(dest='command', required=True)
    for name in ('inspect', 'setup', 'setup-from-receipt', 'run'):
        q = sub.add_parser(name)
        if name == 'setup-from-receipt':
            q.add_argument('inspect', type=Path)
            q.add_argument('dirty', type=Path)
        elif name == 'run':
            q.add_argument('manifest', type=Path)
            q.add_argument('--interval', type=float, default=5)
        else:
            q.add_argument('socket', type=Path)
        q.add_argument('out', type=Path)
    a = p.parse_args()
    if a.command == 'run':
        if a.interval <= 0:
            p.error('interval must be positive')
        run(a.manifest.resolve(), a.out.resolve(), a.interval)
    elif a.command == 'setup-from-receipt':
        save(a.out, setup(load(a.inspect), load(a.dirty)))
    else:
        with ExitStack() as stack:
            guest = Guest(stack, a.socket)
            data = guest.metadata
            if a.command == 'setup':
                guest.state('Paused')
                data = setup(data, guest.dirty())
                guest.state('Paused')
            save(a.out, data)


PFN = (1 << 55) - 1
PRESENT = 1 << 63
SWAP = 1 << 62
COMPLEX = sum(1 << bit for bit in (15, 16, 17, 22))  # compound, huge or THP
ZERO = 1 << 24
UNKNOWN = (1 << 64) - 1


def batches(keys, size):
    """Read only blocks containing requested PFNs (or idle words)."""
    import numpy as np
    for indices in np.split(np.arange(len(keys)), np.flatnonzero(np.diff(keys // size)) + 1):
        if len(indices):
            yield indices[0], indices[-1] + 1


def frame_info(path, pfns):
    import numpy as np
    values = np.full(len(pfns), UNKNOWN, dtype='u8')
    try:
        with open(path, 'rb', buffering=0) as stream:
            for a, b in batches(pfns, 4096):
                base = int(pfns[a])
                n = int(pfns[b - 1]) - base + 1
                raw = os.pread(stream.fileno(), n * 8, base * 8)
                if len(raw) != n * 8:
                    raise OSError(f'short read in {path}')
                values[a:b] = np.frombuffer(raw, dtype='=u8')[pfns[a:b] - base]
        return values, None
    except OSError as exc:
        return values, str(exc)


def idle_bitmap(pfns, arm):
    import numpy as np
    words, inverse = np.unique(pfns // 64, return_inverse=True)
    bits = np.zeros(len(words), dtype='u8')
    bits_per_pfn = np.left_shift(np.uint64(1), pfns % 64)
    if arm:
        np.bitwise_or.at(bits, inverse, bits_per_pfn)
    with open('/sys/kernel/mm/page_idle/bitmap', 'r+b', buffering=0) as stream:
        for a, b in batches(words, 512):
            word = int(words[a])
            n = int(words[b - 1]) - word + 1
            idx = words[a:b] - word
            if arm:
                block = np.zeros(n, dtype='u8')
                block[idx] = bits[a:b]
                if os.pwrite(stream.fileno(), block, word * 8) != n * 8:
                    raise OSError('short idle arm')
            else:
                raw = os.pread(stream.fileno(), n * 8, word * 8)
                if len(raw) != n * 8:
                    raise OSError('short idle read')
                bits[a:b] = np.frombuffer(raw, dtype='=u8')[idx]
    return (bits[inverse] & bits_per_pfn) != 0


def identities(ptes):
    import numpy as np
    return np.where((ptes & PRESENT) != 0, ptes & PFN, np.uint64(0))


def pressure():
    begin = now()
    data = {}
    for filename in ('pressure/cpu', 'pressure/memory', 'pressure/io', 'meminfo', 'vmstat', 'loadavg'):
        try:
            data[filename] = (Path('/proc') / filename).read_text()
        except OSError as exc:
            data[filename] = dict(unavailable=str(exc))
    return dict(begin_ns=begin, end_ns=now(), sources=data)


def snapshots(guests, pool, previous, timing):
    import numpy as np
    records = list(pool.map(lambda v: v.capture(), guests))
    ptes = np.stack([r[0] for r in records])
    frames = identities(ptes)
    pfns = np.unique(frames)
    pfns = pfns[pfns != 0]
    flags, flag_error = frame_info('/proc/kpageflags', pfns)
    counts, count_error = frame_info('/proc/kpagecount', pfns)
    errors = dict(kpageflags=flag_error, kpagecount=count_error)
    # Idle bits represent host references to a frame. Only arm ordinary,
    # exclusive LRU pages, so peer and global-zero traffic cannot look guest-local.
    eligible = pfns[((flags & ((1 << 21) | ZERO | COMPLEX)) == 0) &
                    ((flags & (1 << 5)) != 0) & (counts == 1)]
    marked = np.isin(frames, eligible)
    idle = np.zeros(frames.shape, dtype='u1')
    timing['idle_begin_ns'] = now()
    if previous is not None:
        prior, prior_marked = previous
        observed = marked & prior_marked & (prior == frames)
        probes, inverse = np.unique(frames[observed], return_inverse=True)
        if len(probes):
            try:
                idle[observed] = 1 + idle_bitmap(probes, False)[inverse].astype('u1')
            except OSError as exc:
                errors['idle_read'] = str(exc)
    if len(eligible):
        try:
            idle_bitmap(eligible, True)
        except OSError as exc:
            marked[:] = False
            errors['idle_arm'] = str(exc)
    timing['idle_end_ns'] = now()
    confirm = np.stack(list(pool.map(lambda v: v.pages(), guests)))
    # Ignore unrelated pagemap permission/status bits when rechecking identity.
    stable = ((confirm ^ ptes) & (PRESENT | SWAP | PFN)) == 0
    uncertain_frames = np.union1d(frames[~stable], identities(confirm[~stable]))
    uncertain_frames = uncertain_frames[uncertain_frames != 0]
    idle[~stable] = 0
    marked &= stable
    arrays = dict(ptes=ptes, pfns=pfns, flags=flags, mapcount=counts, stable=np.packbits(stable, axis=1),
                  unstable_pfns=uncertain_frames, idle=idle)
    return arrays, [r[1] for r in records], [r[2] for r in records], (frames, marked), errors


def save_arrays(path, arrays):
    from zipfile import ZipFile, ZIP_DEFLATED
    import numpy as np
    with ZipFile(path, 'x', compression=ZIP_DEFLATED, compresslevel=1) as archive:
        for name, value in arrays.items():
            with archive.open(name + '.npy', 'w') as destination:
                np.lib.format.write_array(destination, value, allow_pickle=False)


def run(manifest_path, out, interval):
    import numpy as np
    config = load(manifest_path)
    rows = config['vms']
    names = [r['name'] for r in rows]
    if len(set(names)) != 4 or len(rows) != 4:
        raise ValueError('expected four named descendants')
    base = load(manifest_path.parent / config['ancestor'])
    setups = [load(manifest_path.parent / r['setup']) for r in rows]
    out.mkdir(mode=0o700, parents=True, exist_ok=True)
    if any((out / f).exists() for f in ('baseline.json', 'metadata.json', 'stop', 'released')):
        raise FileExistsError('use a new collection directory')
    with ExitStack() as stack:
        pool = stack.enter_context(ThreadPoolExecutor(4))
        guests = [Guest(stack, r['socket']) for r in rows]
        if len({v.metadata['pid'] for v in guests}) != 4:
            raise ValueError('duplicate workload VMM')
        for m in setups + [v.metadata for v in guests]:
            if signature(m) != signature(base):
                raise ValueError('guest-physical/image-offset mapping differs from VM base')
        setup_masks = np.stack([dirty_mask(m['dirty'], m) for m in setups])
        save_arrays(out / 'setup.npz', dict(dirty=np.packbits(setup_masks, axis=1)))
        save(out / 'metadata.json', dict(names=names, guests=[v.metadata for v in guests], ancestor=base,
             setup=[{k:v for k,v in m.items() if k != 'dirty'} for m in setups], interval_s=interval,
             clock='CLOCK_MONOTONIC', host=os.uname().nodename, boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip()))
        previous, replay_mask = None, np.zeros_like(setup_masks)
        deadline = now()
        index = 0
        while True:
            live = index != 0
            timing = dict(index=index, begin_ns=now(), scheduled_ns=deadline)
            paused = []
            def pause(v):
                v.state('Running')
                begin = now()
                v.api.request('PATCH', '/vm', 'Paused')
                paused.append(v)
                return dict(request_ns=begin, ack_ns=now())
            def resume(v):
                begin = now()
                v.api.request('PATCH', '/vm', 'Resumed')
                return dict(name=names[guests.index(v)], request_ns=begin, ack_ns=now())
            try:
                if live:
                    jobs = [pool.submit(pause, v) for v in guests]
                    # Let every pause finish before recovering from a peer failure.
                    from concurrent.futures import wait
                    wait(jobs)
                    timing['pause'] = [job.result() for job in jobs]
                timing['aligned_begin_ns'] = now()
                arrays, dirty, charge, previous, errors = snapshots(guests, pool, previous, timing)
                list(pool.map(lambda v: v.state('Paused'), guests))
                timing['pressure'] = pressure()
                timing['aligned_end_ns'] = now()
            finally:
                if paused:
                    timing['resume'] = list(pool.map(resume, paused))
            timing['end_ns'] = now()
            current = np.stack([dirty_mask(raw, v.metadata) for raw, v in zip(dirty, guests)])
            if np.any(replay_mask & ~current):
                raise RuntimeError('stock cumulative replay dirty inventory regressed')
            replay_mask = current
            arrays['replay_dirty'] = np.packbits(current, axis=1)
            save_arrays(out / f'sample-{index:04d}.npz', arrays)
            record = dict(**timing, guest_ram=charge, errors=errors, dirty_pages=current.sum(axis=1).tolist())
            save(out / f'sample-{index:04d}.json', record)
            if index == 0:
                save(out / 'baseline.json', dict(index=0, begin_ns=timing['aligned_begin_ns'],
                                                  end_ns=timing['aligned_end_ns'], archived_ns=now()))
                while not (out / 'released').exists():
                    if (out / 'stop').exists():
                        return
                    time.sleep(.01)
                deadline = now()
            elif stop_requested:
                return
            index += 1
            deadline += int(interval * 1e9)
            stop_requested = False
            while now() < deadline:
                if (out / 'stop').exists():
                    stop_requested = True
                    break
                time.sleep(min(.1, max(0, (deadline - now()) / 1e9)))
            stop_requested |= (out / 'stop').exists()
            if now() - deadline > interval * 1e9:
                deadline = now()


if __name__ == '__main__':
    main()
