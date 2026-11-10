import json
from pathlib import Path
import tempfile
import unittest
from suite import observation_memory

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

if __name__ == "__main__": unittest.main()
