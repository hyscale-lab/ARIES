#!/usr/bin/env python3
"""Verify full guest-flag coverage of every selected RAM frame in a TeX diagnostic."""
import argparse
import hashlib
import json
from bisect import bisect_right
from collections import Counter
from pathlib import Path

from snapshot_probe import selected


def verify(raw, count):
    boundaries = []
    baseline_frames = None
    endpoints = []
    for line in (raw / 'idle.jsonl').open():
        event = json.loads(line)
        assert event['status'] == 'ok'
        guest = event['guest_telemetry']['guest']
        scan = guest['pfn_scan']
        assert guest['page_bytes'] == 4096 and '/proc/kpageflags' not in guest['source_errors']
        assert scan['state'] == 'complete' and scan['requested_start_pfn'] == 0
        assert scan['observed_end_pfn_exclusive'] == scan['requested_pfn_count'] == count
        ranges = scan['ranges']
        starts = [r['start_pfn'] for r in ranges]
        assert starts[0] == 0 and ranges[-1]['end_pfn_exclusive'] == count
        assert starts[1:] == [r['end_pfn_exclusive'] for r in ranges[:-1]]
        slots = sorted(event['ram_slots'], key=lambda s: s['guest_phys_addr'])
        assert sum(s['memory_size'] for s in slots) == 4096 * (1 << 20)
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
            assert 0 <= begin < end <= count
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
                               guest_ranges=len(ranges), observed_end_pfn_exclusive=count,
                               sample_count=len(frames), slots=covered_slots,
                               guest_probe_ns=event['guest_telemetry']['host_end_ns']-event['guest_telemetry']['host_begin_ns']))
    assert boundaries[0]['phase'] == 'session-start' and boundaries[-1]['phase'] == 'session-end'
    assert endpoints == ['session-end'] and len(boundaries) == 20
    frame_hash = hashlib.sha256(b''.join(f.to_bytes(8, 'little') for f in baseline_frames)).hexdigest()
    return dict(run_id=raw.name, guest_pfn_count=count, sample_frame_ids_le_u64_sha256=frame_hash,
                boundaries=boundaries, snapshot_comparison_phases=endpoints)


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('raw', type=Path)
    p.add_argument('--guest-pfn-count', type=int, default=1310720)
    args = p.parse_args()
    print(json.dumps(verify(args.raw, args.guest_pfn_count), indent=2))
