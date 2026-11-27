#!/usr/bin/env python3
"""Reduce a paused four-VM pagemap capture. All capacity columns count 4 KiB pages.

The logical k-1 bound includes known zero pages. The d-1 partition uses physical
identities only where all k PFNs are visible. The 'ordinary' subset also requires
normal frames with no extra mappings. Each partition charges an offset once.
"""
import argparse
import csv
import json
from pathlib import Path

import numpy as np

PFN = (1 << 55) - 1
PRESENT = 1 << 63
SWAP = 1 << 62
ZERO = 1 << 24
COMPLEX = sum(1 << bit for bit in (15, 16, 17, 22))
UNKNOWN = (1 << 64) - 1


def reduce_arrays(data, setup):
    ptes = data['ptes']
    n = ptes.shape[1]
    if ptes.shape != (4, n) or n == 0 or setup.shape != (4, n):
        raise ValueError('invalid cohort dimensions')
    def unpack(key):
        return np.unpackbits(data[key], axis=1, count=n).astype(bool)
    replay = unpack('replay_dirty')
    stable = unpack('stable')
    present = (ptes & PRESENT) != 0
    tracked = stable.all(axis=0) & ~(present & ((ptes & SWAP) != 0)).any(axis=0)
    clean = present & ~replay & ~setup & tracked
    k = clean.sum(axis=0).astype('i1')
    logical = np.maximum(k - 1, 0)
    active = k >= 2
    raw = np.where(present, ptes & PFN, np.uint64(0))
    frames = np.where(clean, raw, np.uint64(0))
    masked = (clean & (raw == 0)).any(axis=0)
    identified = active & ~masked
    d = np.zeros(n, dtype='i1')
    for i, frame in enumerate(frames):
        d += (frame != 0) & ~(frames[:i] == frame).any(axis=0)
    physical = np.maximum(d - 1, 0)
    ids = data['pfns']
    if (len(ids) and (ids[0] == 0 or np.any(ids[:-1] >= ids[1:]))) or not np.array_equal(
            ids, np.unique(raw[raw != 0])):
        raise ValueError('PFN dictionary disagrees with observed mappings')
    # Index zero represents absent or masked. Count every occurrence in all guest
    # RAM slots, including dirty and different-offset aliases, before checking the global mapcount.
    idx = np.where(raw == 0, 0, 1 + np.searchsorted(ids, raw)).astype(np.intp)
    cohort = np.bincount(idx.ravel(), minlength=len(ids) + 1)
    flags = np.r_[np.uint64(UNKNOWN), data['flags']]
    counts = np.r_[np.uint64(UNKNOWN), data['mapcount']]
    if len(flags) != len(cohort) or len(counts) != len(cohort):
        raise ValueError('incomplete frame metadata')
    zero = (clean & (flags[idx] != UNKNOWN) & ((flags[idx] & ZERO) != 0)).any(axis=0)
    weak = ((flags == UNKNOWN) | (counts == UNKNOWN) | (counts < cohort) | ((flags & COMPLEX) != 0))
    weak[1:] |= np.isin(ids, data['unstable_pfns'])
    external = (clean & ~weak[idx] & (counts[idx] > cohort[idx])).any(axis=0)
    aliases = np.zeros(n, dtype=bool)
    for frame, row in zip(frames, idx):
        aliases |= (frame != 0) & (cohort[row] > (frames == frame).sum(axis=0))
    complex_or_unknown = (clean & weak[idx]).any(axis=0)
    # Flags can overlap. Assign in this order and retain independent incidence below.
    remaining = identified.copy()
    groups = {}
    for label, mask in [('zero', zero), ('frame_metadata_unresolved', complex_or_unknown),
                        ('external', external), ('other_cohort_alias', aliases), ('ordinary', active)]:
        group = remaining & mask
        groups[label] = group
        remaining &= ~group
    result = dict(tracked_offsets=int(tracked.sum()), unstable_or_unresolved_offsets=int((~tracked).sum()),
                  eligible_mappings=int(k.sum()), logical_pages=int(logical.sum()),
                  masked_logical_pages=int(logical[active & masked].sum()),
                  pfn_visible_logical_pages=int(logical[identified].sum()),
                  already_shared_pages=int((k[identified] - d[identified]).sum()),
                  pfn_d_minus_one_pages=int(physical[identified].sum()),
                  one_distinct_frame_offsets=int((identified & (d == 1)).sum()),
                  observed_zero_mappings=int((present & (flags[idx] != UNKNOWN) & ((flags[idx] & ZERO) != 0)).sum()),
                  pfn_masked_resident_mappings=int((present & (raw == 0)).sum()),
                  external_candidate_offsets=int((identified & external).sum()),
                  other_cohort_alias_candidate_offsets=int((identified & aliases).sum()))
    for label, group in groups.items():
        result[label + '_offsets'] = int(group.sum())
        result[label + '_logical_pages'] = int(logical[group].sum())
        result[label + '_d_minus_one_pages'] = int(physical[group].sum())
    for count in range(5):
        result[f'k{count}_offsets'] = int(((k == count) & tracked).sum())
    # Ask whether *all* contributing mappings were observed idle, any accessed,
    # or lacked enough history. Retain the same d-1 charge for that offset.
    idle = data['idle']
    all_idle = (~clean | (idle == 2)).all(axis=0)
    any_access = (clean & (idle == 1)).any(axis=0)
    for name, mask in [('all_idle', all_idle), ('any_access', any_access),
                       ('idle_unresolved', ~all_idle & ~any_access)]:
        result[name + '_ordinary_pages'] = int(physical[groups['ordinary'] & mask].sum())
        result[name + '_logical_pages'] = int(logical[mask].sum())
    if (result['logical_pages'] != result['masked_logical_pages'] + result['already_shared_pages'] + result['pfn_d_minus_one_pages']
        or result['pfn_d_minus_one_pages'] != sum(result[k + '_d_minus_one_pages'] for k in groups)):
        raise ValueError('inconsistent copy accounting')
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('run', type=Path)
    p.add_argument('csv', type=Path)
    a = p.parse_args()
    meta = json.loads((a.run / 'metadata.json').read_text())
    pages = (meta['ancestor']['machine']['mem_size_mib'] << 20) // 4096
    with np.load(a.run / 'setup.npz') as raw:
        setup = np.unpackbits(raw['dirty'], axis=1, count=pages).astype(bool)
    records = []
    for path in sorted(a.run.glob('sample-*.json')):
        sample = json.loads(path.read_text())
        with np.load(path.with_suffix('.npz')) as data:
            result = reduce_arrays(data, setup)
        charge = sample['guest_ram']
        row = dict(index=sample['index'], aligned_begin_ns=sample['aligned_begin_ns'],
                   aligned_end_ns=sample['aligned_end_ns'], sum_rss_bytes=sum(r['Rss_bytes'] for r in charge),
                   sum_pss_bytes=sum(r['Pss_bytes'] for r in charge), **result)
        for name, metrics in zip(meta['names'], charge, strict=True):
            for key in ('Rss_bytes', 'Pss_bytes'):
                row[name + '_' + key.lower()] = metrics[key]
        records.append(row)
    if not records or [r['index'] for r in records] != list(range(len(records))):
        raise ValueError('missing baseline or sample')
    with a.csv.open('w', newline='') as stream:
        writer = csv.DictWriter(stream, fieldnames=records[0].keys(), lineterminator='\n')
        writer.writeheader()
        writer.writerows(records)


if __name__ == '__main__':
    main()
