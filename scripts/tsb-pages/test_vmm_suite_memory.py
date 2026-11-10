import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from suite import observation_memory, Suite

class MemoryTests(unittest.TestCase):
    def test_authoritative_regions_without_legacy_slot_capture(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "access-dirty").mkdir()
            regions = [{"slot": 0, "guest_base": 0, "bytes": 4096, "page_count": 1},
                       {"slot": 1, "guest_base": 8192, "bytes": 8192, "page_count": 2}]
            initial = {"schema": "aries.firecracker.trace-epochs.v1",
                       "vm_generation": "g", "baseline_discarded": True, "regions": regions}
            (root / "access-dirty/initial-arm.json").write_text(json.dumps(initial))
            groups = [{"reading": {"vm_generation": "g", "regions": regions}}]
            self.assertEqual(observation_memory(root, groups, "vmm_owned"),
                             (12288, "vmm_initial_arm_authoritative_regions"))
            groups[0]["reading"]["vm_generation"] = "different"
            with self.assertRaises(ValueError): observation_memory(root, groups, "vmm_owned")

    def test_overlap_cannot_pass_native_memory_sum(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "access-dirty").mkdir()
            regions = [{"slot": 0, "guest_base": 0, "bytes": 8192, "page_count": 2},
                       {"slot": 1, "guest_base": 4096, "bytes": 4096, "page_count": 1}]
            initial = {"schema": "aries.firecracker.trace-epochs.v1",
                       "vm_generation": "g", "baseline_discarded": True, "regions": regions}
            (root / "access-dirty/initial-arm.json").write_text(json.dumps(initial))
            with self.assertRaises(ValueError): observation_memory(root, [], "vmm_owned")

class CaptureSelectionTests(unittest.TestCase):
    def fixture(self, root, *, backend="vmm_owned", stale=False, other_vm=False):
        observer = root / "task/observer"; observer.mkdir(parents=True)
        pid = os.getpid()
        start = int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])
        identity = {"pid": pid, "start_ticks": start + int(stale),
                    "vm_id": "aries-sandbox-task-" + ("other" if other_vm else "fixture")}
        (observer / "vmm.json").write_text(json.dumps(identity))
        (observer / "access-dirty").mkdir()
        initial = {"schema": "aries.firecracker.trace-epochs.v1", "vm_generation": "g",
                   "baseline_discarded": True,
                   "regions": [{"slot": 0, "guest_base": 0, "bytes": 8192, "page_count": 2}]}
        (observer / "access-dirty/initial-arm.json").write_text(json.dumps(initial))
        config = root / "config.json"; config.write_text(json.dumps({"backend": backend}))
        if backend != "vmm_owned":
            (observer / "selected-slots.json").write_text(json.dumps([{"memory_size": 8192}]))
        suite = object.__new__(Suite); suite.root = root
        suite.manifest = {"execution": {"access_dirty_config": {"path": str(config)}}}
        events = []; suite.event = lambda *args, **kwargs: events.append((args, kwargs))
        status = {"task_id": "task", "vm_prefix": "aries-sandbox-task-"}
        original = Path.read_bytes
        def read_bytes(path):
            if str(path) == f"/proc/{pid}/cmdline":
                return b"firecracker\0--id\0aries-sandbox-task-fixture\0"
            return original(path)
        with patch.object(Path, "read_bytes", read_bytes):
            suite.refresh_capture(status)
        return status, events, observer

    def test_vmm_capture_uses_authoritative_regions_without_strace_artifacts(self):
        with tempfile.TemporaryDirectory() as directory:
            status, events, observer = self.fixture(Path(directory))
            self.assertFalse((observer / "selected-slots.json").exists())
            capture = status["capture"]
            self.assertEqual(capture["selected_slot_memory_bytes"], 8192)
            self.assertEqual(capture["vm_generation"], "g")
            self.assertEqual(capture["memory_region_evidence"], "vmm_initial_arm_authoritative_regions")
            self.assertNotIn("strace_detached_before_capture_publication", capture)
            self.assertEqual(len(events), 1)

    def test_reused_pid_and_other_vm_identity_cannot_publish_capture(self):
        for mismatch in ({"stale": True}, {"other_vm": True}):
            with self.subTest(mismatch=mismatch), tempfile.TemporaryDirectory() as directory:
                status, events, _ = self.fixture(Path(directory), **mismatch)
                self.assertNotIn("capture", status)
                self.assertEqual(events, [])

    def test_legacy_capture_keeps_its_original_slot_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            status, events, _ = self.fixture(Path(directory), backend="legacy_external")
            capture = status["capture"]
            self.assertEqual(capture["selected_slot_memory_bytes"], 8192)
            self.assertTrue(capture["strace_detached_before_capture_publication"])
            self.assertIn("selected_slots_sha256", capture)
            self.assertEqual(len(events), 1)


if __name__ == "__main__": unittest.main()
