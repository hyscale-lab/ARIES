#!/usr/bin/env python3
"""Check the actual compiler/test outputs, including any separately logged late result."""
from collections import Counter
import hashlib
import json
from pathlib import Path
import sys


def check(trajectory, events):
    source = trajectory.read_bytes()
    assert hashlib.sha256(source).hexdigest() == '691a76e8f280185c4627faeadc5acae4896ec0529308447f5013b1307052b3d5'
    observations = {r['source_call_id']: r['content'] for s in json.loads(source)['steps']
                    for r in s.get('observation', {}).get('results', [])}
    with events.open() as stream:
        rows = [json.loads(line) for line in stream]
    checks = []
    for sequence in (4, 9, 10):
        reply, = [r for r in rows if r['event'] == 'tool_reply' and r['sequence'] == sequence]
        call = reply['source_call_id']
        parts = [reply] + [r for r in rows if r['event'] == 'diagnostic_process_observation' and r['source_call_id'] == call]
        _, marker, text = observations[call].partition('\nOutput:\n')
        assert marker
        expected = Counter(text.splitlines())
        # stdout and stderr are separate; rg's file iteration order varies across filesystems.
        actual = Counter()
        for channel in ('stdout', 'stderr'):
            actual.update(''.join(p.get(channel, '') for p in parts).splitlines())
        assert actual == expected, (sequence, expected - actual, actual - expected)
        assert parts[-1]['exit_code'] == reply['expected_exit_code']
        checks.append(dict(sequence=sequence, source_call_id=call, matched_output_lines=actual.total(),
                           first_exit_code=reply['exit_code'], terminal_exit_code=parts[-1]['exit_code'],
                           sorted_lines_sha256=hashlib.sha256(json.dumps(sorted(actual.elements())).encode()).hexdigest()))
    return checks


if __name__ == '__main__':
    print(json.dumps(check(Path(sys.argv[1]), Path(sys.argv[2])), indent=2))
