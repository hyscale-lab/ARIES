"""Recognize a completed old execution from its actual source and trace receipts."""
import json
from pathlib import Path
import sys

raw, expected_path, short = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
expected = json.loads(expected_path.read_bytes())[short]
try:
    if (raw / "driver.exit").read_text().strip() != "0":
        sys.exit(1)
    with (raw / "events.jsonl").open() as stream:
        events = list(map(json.loads, stream))
    first, last = events[0], events[-1]
    assert [first[key] for key in ("trial_id", "trajectory_sha256", "rollout_sha256")] == [
        expected["trial"][key] for key in ("trial", "trajectory_sha256", "rollout_sha256")
    ]
    assert last["event"] == "removed" and any(e["event"] == "final_task_state" for e in events)
    assert sum(e["event"] == "tool_reply" for e in events) == expected["guest_calls"]
    assert sum(e["event"] == "tool_dispatch" for e in events) == expected["guest_calls"]
    assert all(e["exit_code"] == e["expected_exit_code"] for e in events if e["event"] == "tool_reply")
    for name in ("idle.jsonl", "samples.jsonl"):
        assert (raw / name).stat().st_size > 0
except (OSError, AssertionError, IndexError, KeyError, ValueError):
    sys.exit(1)
