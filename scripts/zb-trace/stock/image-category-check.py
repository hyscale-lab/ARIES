#!/usr/bin/env python3
"""Check six live replies and 100 result files against the pinned image-category source."""
from collections import Counter
from datetime import datetime
from hashlib import file_digest, sha256
import json
from pathlib import Path, PurePosixPath
import sys
import tarfile


def verify_digest(path, expected):
    with path.open('rb') as source:
        assert file_digest(source, 'sha256').hexdigest() == expected, path


def rows(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


def main(trace, fixture, run):
    source_sha = '2543f544fef10d6ee0da62095eb37bdfc9ba9f4d5998db798e7a54e85c0918ab'
    fixture_sha = 'f9ebc183b76d8f2ec58e9c0160a2f07e86fa6460d5febd786a2bf735f3368946'
    verify_digest(trace, source_sha)
    verify_digest(fixture, fixture_sha)
    source = rows(trace)
    messages = [row['message'] for row in source if 'message' in row]
    calls = [call for message in messages if message['role'] == 'assistant'
             for call in message['content'] if call['type'] == 'toolCall']
    replies = [message for message in messages if message['role'] == 'toolResult']
    assert len(calls) == len(replies) == 6
    assert all(call['name'] == 'exec' for call in calls)
    assert [call['id'] for call in calls] == [reply['toolCallId'] for reply in replies]
    replay = rows(run/'events.jsonl')

    def events(name):
        return [row for row in replay if row['event'] == name]

    config, = events('replay_configuration')
    assert config['benchmark_evaluator_enabled'] is False
    assert config['runtime'] == 'runc' and config['container'] == 'zb-workload'
    assert config['openclaw_trace']['sha256'] == source_sha
    start, = events('session_start')
    end, = events('session_end')
    assert not start.get('source_playback')
    dispatches, observed_replies = events('tool_dispatch'), events('tool_reply')
    originals = [row for row in events('source_event') if row.get('source_kind') == 'toolResult']
    assert len(dispatches) == len(observed_replies) == len(originals) == len(calls)
    results, covered, tool_union = [], 0, 0
    for i, (call, reply, dispatch, observed, original) in enumerate(zip(calls, replies, dispatches, observed_replies, originals), 1):
        assert dispatch['sequence'] == observed['sequence'] == original['sequence'] == i
        assert dispatch['operation'] == observed['operation'] == 'Exec'
        assert dispatch['source_call_id'] == observed['source_call_id'] == original['source_call_id'] == call['id']
        assert not dispatch.get('source_process_session_id') and not observed.get('source_process_session_id')
        exit_code = reply['details']['exitCode']
        assert original['source_reply_status'] == reply['details']['status'] == 'completed'
        assert original['source_reply_is_error'] == reply['isError'] is False
        assert 'command.started' in observed['guest_process_timing']['milestones']
        assert 'native_error' not in observed
        expected = reply['details']['aggregated'].removesuffix(f'\n\n(Command exited with code {exit_code})') if exit_code else reply['details']['aggregated']
        actual = observed.get('stdout', '') + observed.get('stderr', '')
        begin, finish = dispatch['monotonic_ns'], observed['monotonic_ns']
        assert start['monotonic_ns'] <= begin <= finish <= end['monotonic_ns']
        tool_union += max(0, finish - max(begin, covered))
        covered = max(covered, finish)
        results.append(dict(sequence=i, source_call_id=call['id'], source_exit_code=exit_code,
                            observed_exit_code=observed['exit_code'], source_status=original['source_reply_status'],
                            output_matches=actual.rstrip('\n') == expected.rstrip('\n'),
                            source_output_sha256=sha256(expected.encode()).hexdigest(),
                            actual_output_sha256=sha256(actual.encode()).hexdigest(),
                            source_output_bytes=len(expected.encode()), actual_output_bytes=len(actual.encode()),
                            dispatch_ns=begin, reply_ns=finish))

    modeled_waits, last = [], None
    for row in source:
        if last is None and row.get('message', {}).get('role') != 'user':
            continue
        now = datetime.fromisoformat(row['timestamp'])
        if last is not None and row.get('message', {}).get('role') != 'toolResult' and now > last:
            modeled_waits.append(round((now-last).total_seconds() * 1_000_000) * 1_000)
        last = now
    wait_begin, wait_end = events('wait_start'), events('wait_end')
    assert [row['requested_ns'] for row in wait_begin] == [row['requested_ns'] for row in wait_end] == modeled_waits
    observed_waits = [end['monotonic_ns'] - begin['monotonic_ns'] for begin, end in zip(wait_begin, wait_end)]
    for wait_start, wait_finish, requested in zip(wait_begin, wait_end, modeled_waits):
        assert wait_finish['monotonic_ns'] - wait_start['monotonic_ns'] >= requested - 1_000_000
    tool, = events('live_tool_time')
    assert tool['guest_calls'] == 6 and tool['dispatch_to_first_reply_union_ns'] == tool_union

    with tarfile.open(fixture) as archive:
        images = {}
        for member in archive:
            if member.name.startswith('._'):
                continue
            assert member.isfile() and member.name == PurePosixPath(member.name).name and member.name not in images
            with archive.extractfile(member) as image:
                images[member.name] = file_digest(image, 'sha256').hexdigest()
    paths = [line for line in replies[-1]['details']['aggregated'].splitlines() if line.startswith('/tmp_workspace/results/')]
    assert len(paths) == len(set(paths)) == len(images) == 100
    expected_hashes = {path: images[PurePosixPath(path).name] for path in paths}
    actual_hashes = {}
    for line in (run/'output.sha256').read_text().splitlines():
        digest, path = line.split('  ', 1)
        assert path not in actual_hashes
        actual_hashes[path] = digest
    missing = sorted(expected_hashes.keys() - actual_hashes.keys())
    extra = sorted(actual_hashes.keys() - expected_hashes.keys())
    modified = sorted(path for path in expected_hashes.keys() & actual_hashes.keys() if expected_hashes[path] != actual_hashes[path])
    print(json.dumps(dict(trace_sha256=source_sha, fixture_sha256=fixture_sha, benchmark_evaluator_enabled=False,
                          calls=results, wait_targets_ns=modeled_waits, observed_wait_ns=observed_waits,
                          elapsed_ns=end['monotonic_ns']-start['monotonic_ns'], live_tool_union_ns=tool_union,
                          expected_files=len(expected_hashes), observed_files=len(actual_hashes),
                          files_by_category=dict(sorted(Counter(PurePosixPath(path).parent.name for path in actual_hashes).items())),
                          missing_files=missing, extra_files=extra, changed_files=modified), indent=2))
    return int(bool(missing or extra or modified or any(row['observed_exit_code'] != row['source_exit_code'] for row in results)))


if __name__ == '__main__':
    sys.exit(main(*map(Path, sys.argv[1:])))
