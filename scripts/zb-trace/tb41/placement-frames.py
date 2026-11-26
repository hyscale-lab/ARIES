#!/usr/bin/env python3
"""Audit overlap of two live guests' identifiable resident frames without reading page contents."""
from argparse import ArgumentParser
from array import array
import json
import os
from pathlib import Path
import time

from placement import inspect
from snapshot_probe import PAGE, PFN_MASK, PRESENT


def resident_frames(metadata):
    resident = set()
    with open(f"/proc/{metadata['vmm_pid']}/pagemap", 'rb', buffering=0) as pm:
        for slot in metadata['ram_slots']:
            size = slot['memory_size'] // PAGE * 8
            offset = slot['userspace_addr'] // PAGE * 8
            data = os.pread(pm.fileno(), size, offset)
            if len(data) != size:
                raise RuntimeError('short read for a KVM memory slot')
            pages = array('Q')
            pages.frombytes(data)
            resident.update(v & PFN_MASK for v in pages if v & PRESENT and v & PFN_MASK)
    return resident


def audit(unit, runtime, peer, runs):
    begin = time.monotonic_ns()
    live = inspect(unit, runtime, peer, require_pair=True)
    identities = {(v['pid'], int(v['start_ticks'])) for v in live['vmms']}
    metadata = [json.loads((p/'native-deep/metadata.json').read_bytes()) for p in runs]
    if {(m['vmm_pid'], m['vmm_start_ticks']) for m in metadata} != identities:
        raise ValueError('collector PIDs or start times differ from the socket peers')
    left, right = map(resident_frames, metadata)
    overlap = left & right
    shared_or_zero = nonexclusive = 0
    with open('/proc/kpageflags', 'rb', buffering=0) as flags, open('/proc/kpagecount', 'rb', buffering=0) as count:
        for pfn in overlap:
            page_flags = int.from_bytes(os.pread(flags.fileno(), 8, pfn * 8), 'little')
            map_count = int.from_bytes(os.pread(count.fileno(), 8, pfn * 8), 'little')
            shared_or_zero += bool(page_flags & ((1 << 21) | (1 << 24)))
            nonexclusive += map_count != 1
    after = inspect(unit, runtime, peer, require_pair=True)
    if identities != {(v['pid'], int(v['start_ticks'])) for v in after['vmms']}:
        raise ValueError('socket identity changed during frame audit')
    return dict(placement=live, start_ns=begin, end_ns=time.monotonic_ns(), resident_unique_pfns=[len(left), len(right)],
                observed_intersection=len(overlap), shared_or_zero=shared_or_zero,
                overlap_nonexclusive_mapcount=nonexclusive)


if __name__ == '__main__':
    parser = ArgumentParser(description=__doc__)
    parser.add_argument('unit')
    parser.add_argument('runtime', type=Path)
    parser.add_argument('peer')
    parser.add_argument('runs', type=Path, nargs=2)
    a = parser.parse_args()
    print(json.dumps(audit(a.unit, a.runtime, a.peer, a.runs)))
