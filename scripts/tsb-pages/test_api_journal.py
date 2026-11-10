"""Receipt persistence must scale with new calls and retain failures."""
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from vmm_observer import VmmObserver


class JournalTests(unittest.TestCase):
    def observer(self, root):
        observer = object.__new__(VmmObserver)
        observer.output = root; observer.vm_id = "vm"; observer.pid = 1; observer.start = 2
        observer.invalid = False; observer.events = io.StringIO()
        observer.api = SimpleNamespace(calls=[])
        return observer

    def test_each_receipt_is_appended_once_and_old_history_is_not_rewritten(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); observer = self.observer(root)
            observer.api.calls = [{"operation": "export", "begin_ns": i, "end_ns": i + 1}
                                  for i in range(64)]
            observer._event("arm", lambda event: None)
            journal = root / "api-calls.jsonl"; before = journal.read_bytes()
            self.assertEqual(observer.api.calls, [])
            observer.api.calls.append({"operation": "ack", "begin_ns": 100, "end_ns": 101})
            observer._event("collect", lambda event: None)
            after = journal.read_bytes(); rows = [json.loads(line) for line in after.splitlines()]
            self.assertTrue(after.startswith(before))
            self.assertEqual([row["sequence"] for row in rows], list(range(1, 66)))
            self.assertEqual(rows[-1]["operation"], "ack")
            self.assertEqual(observer.api.calls, [])
            events = [json.loads(line) for line in observer.events.getvalue().splitlines()]
            self.assertEqual([event["api_journal_records"] for event in events], [64, 1])
            self.assertEqual(events[-1]["api_journal_bytes"], len(after) - len(before))
            self.assertLess(len(after) - len(before), len(before) // 32)
            self.assertFalse((root / "api-calls.json").exists())

    def test_action_failure_keeps_receipts_and_invalid_outcome(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); observer = self.observer(root)
            def fail(event):
                observer.api.calls.append({"operation": "rotate", "outcome": "failed"})
                raise RuntimeError("native failure")
            with self.assertRaisesRegex(RuntimeError, "native failure"):
                observer._event("collect", fail)
            self.assertTrue(observer.invalid)
            self.assertEqual(json.loads((root / "api-calls.jsonl").read_text())["outcome"], "failed")
            self.assertEqual(json.loads(observer.events.getvalue())["error"]["message"], "native failure")

    def test_failed_durable_write_does_not_drop_the_buffer(self):
        with tempfile.TemporaryDirectory() as directory:
            observer = self.observer(Path(directory))
            observer.api.calls.append({"operation": "rotate"})
            with patch("vmm_observer.os.fsync", side_effect=OSError("fixture full disk")):
                with self.assertRaisesRegex(OSError, "full disk"):
                    observer._event("collect", lambda event: None)
            self.assertTrue(observer.invalid)
            self.assertEqual(observer.api.calls, [{"operation": "rotate"}])
            self.assertIn("persistence_error", json.loads(observer.events.getvalue()))


if __name__ == "__main__":
    unittest.main()
