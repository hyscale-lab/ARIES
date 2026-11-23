#!/usr/bin/env python3
"""Verify full guest-flag coverage of every selected RAM frame in a diagnostic."""
import argparse
import hashlib
import json
from bisect import bisect_right
from collections import Counter
from pathlib import Path

from snapshot_probe import selected


def verify(raw, count, memory_mib=4096, boundary_count=20, *, allow_eof_after_ram=False):
    boundaries = []
    baseline_frames = None
    endpoints = []
    for line in (raw / 'idle.jsonl').open():
        event = json.loads(line)
        assert event['status'] == 'ok'
        guest = event['guest_telemetry']['guest']
        scan = guest['pfn_scan']
        assert guest['page_bytes'] == 4096 and '/proc/kpageflags' not in guest['source_errors']
        assert scan['requested_start_pfn'] == 0 and scan['requested_pfn_count'] == count
        observed = scan['observed_end_pfn_exclusive']
        if allow_eof_after_ram and scan['state'] == 'end_of_kpageflags':
            assert 0 < observed < count
        else:
            assert scan['state'] == 'complete' and observed == count
        ranges = scan['ranges']
        starts = [r['start_pfn'] for r in ranges]
        assert starts[0] == 0 and ranges[-1]['end_pfn_exclusive'] == observed
        assert starts[1:] == [r['end_pfn_exclusive'] for r in ranges[:-1]]
        slots = sorted(event['ram_slots'], key=lambda s: s['guest_phys_addr'])
        assert sum(s['memory_size'] for s in slots) == memory_mib * (1 << 20)
        frames = [r['gfn'] for r in event['content_sample']]
        assert len(frames) == len(set(frames)) == 8192
        expected = [gfn for gfn, _, _ in selected(slots, len(frames))]
        assert frames == expected
        if baseline_frames is None:
            baseline_frames = frames
        assert frames == baseline_frames
        if 'original_snapshot_sample' in event:
            assert [r['gfn'] for r in event['original_snapshot_sample']] == frames
            endpoints.append(event['phase'])
        covered_slots = []
        for slot in slots:
            begin = slot['guest_phys_addr'] // 4096
            end = begin + slot['memory_size'] // 4096
            assert 0 <= begin < end <= observed
            classes = Counter()
            for frame in frames:
                if begin <= frame < end:
                    r = ranges[bisect_right(starts, frame) - 1]
                    assert r['start_pfn'] <= frame < r['end_pfn_exclusive']
                    classes[r['class']] += 1
            assert classes
            covered_slots.append(dict(begin_gfn=begin, end_gfn_exclusive=end,
                                      covered_sampled_frames=sum(classes.values()),
                                      sampled_flag_classes=dict(classes)))
        assert sum(s['covered_sampled_frames'] for s in covered_slots) == len(frames)
        boundaries.append(dict(phase=event['phase'], step=event['step'], scan_state=scan['state'],
                               guest_ranges=len(ranges), observed_end_pfn_exclusive=observed,
                               sample_count=len(frames), slots=covered_slots,
                               guest_probe_ns=event['guest_telemetry']['host_end_ns']-event['guest_telemetry']['host_begin_ns']))
    assert boundaries[0]['phase'] == 'session-start' and boundaries[-1]['phase'] == 'session-end'
    assert endpoints == ['session-end'] and len(boundaries) == boundary_count
    frame_hash = hashlib.sha256(b''.join(f.to_bytes(8, 'little') for f in baseline_frames)).hexdigest()
    return dict(run_id=raw.name, guest_pfn_count=count, sample_frame_ids_le_u64_sha256=frame_hash,
                boundaries=boundaries, snapshot_comparison_phases=endpoints)


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('raw', type=Path)
    p.add_argument('--guest-pfn-count', type=int, default=1310720)
    p.add_argument('--memory-mib', type=int, default=4096)
    p.add_argument('--boundaries', type=int, default=20)
    p.add_argument('--allow-eof-after-ram', action='store_true')
    args = p.parse_args()
    print(json.dumps(verify(args.raw, args.guest_pfn_count, args.memory_mib, args.boundaries,
                            allow_eof_after_ram=args.allow_eof_after_ram), indent=2))
