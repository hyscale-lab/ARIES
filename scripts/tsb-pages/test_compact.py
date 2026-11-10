"""Contradictory retained evidence cannot pass individual bitmap hash checks."""
import copy
import hashlib
import sys
from pathlib import Path
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0,str(ROOT/"scripts/tsb-companion"))
from compact import expand_reading


class CompactTests(unittest.TestCase):
    def reading(self, root, **changes):
        values = {"access_observed": 1, "access_unknown": 2, "unknown_not-on-lru": 2,
                  "access_idle": 4, "access_absent": 8,
                  "kvm_dirty": 4, "vmm_dirty": 1, "combined_dirty": 5}
        values.update(changes)
        masks = {}
        for name, bits in values.items():
            raw = bits.to_bytes(8, "little")
            (root / (name + ".bin")).write_bytes(raw)
            masks[name] = {"file": name + ".bin", "pages": bits.bit_count(),
                           "sha256": hashlib.sha256(raw).hexdigest()}
        return {"schema": "aries.vmm-tracer.compact.v1", "page_bytes": 4096,
                "vm_instance": "vm", "artifact_directory": str(root),
                "regions": [{"slot": 0, "guest_base": 0, "bytes": 16384, "masks": masks}],
                "observed_pages": 1, "unknown_pages": 1, "dirty_pages": 2, "counted_pages": 3}

    def reject(self, reading, reason):
        with self.assertRaisesRegex(ValueError, reason):
            expand_reading(reading)

    def test_valid_partition_and_conservative_union_preserve_raw_input(self):
        with tempfile.TemporaryDirectory() as temporary:
            reading = self.reading(Path(temporary)); before = copy.deepcopy(reading)
            result = expand_reading(reading)
            self.assertEqual(result["accessed_gpas"], [0])
            self.assertEqual(result["idle_gpas"], [8192])
            self.assertEqual(result["unknown_gpas"], {"4096": "not-on-lru"})
            self.assertEqual(result["dirty_gpas"], [0, 8192])
            self.assertEqual(result["additional_access_sources"],
                {"0": ["vmm_potential_write"], "8192": ["kvm_dirty"]})
            self.assertEqual(reading, before)

    def test_individually_hashed_contradictory_masks_are_rejected(self):
        cases = [({"combined_dirty": 4}, "dirty union"),
                 ({"combined_dirty": 7}, "dirty union"),
                 ({"access_idle": 6}, "classifications overlap"),
                 ({"access_absent": 0}, "cover RAM"),
                 ({"unknown_other": 2}, "unknown reasons overlap"),
                 ({"unknown_not-on-lru": 0}, "unknown-reason partition")]
        for changes, reason in cases:
            with self.subTest(changes=changes), tempfile.TemporaryDirectory() as temporary:
                self.reject(self.reading(Path(temporary), **changes), reason)

    def test_region_aliases_and_missing_required_evidence_are_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            original = self.reading(Path(temporary))
            for field in ("access_unknown", "combined_dirty", "access_absent"):
                reading = copy.deepcopy(original); del reading["regions"][0]["masks"][field]
                self.reject(reading, "required masks")
            reading = copy.deepcopy(original); reading["regions"] *= 2
            self.reject(reading, "overlapping")
            reading = copy.deepcopy(original); reading["regions"][0]["guest_base"] = 1
            self.reject(reading, "invalid compact RAM")

    def test_summary_must_agree_with_verified_masks(self):
        with tempfile.TemporaryDirectory() as temporary:
            original = self.reading(Path(temporary))
            for key in ("observed_pages", "unknown_pages", "dirty_pages", "counted_pages"):
                reading = {**original, key: original[key] + 1}
                self.reject(reading, "summary/mask count")

    def test_legacy_archive_can_omit_unmeasured_absence(self):
        with tempfile.TemporaryDirectory() as temporary:
            reading = self.reading(Path(temporary)); reading.pop("schema")
            del reading["regions"][0]["masks"]["access_absent"]
            self.assertEqual(expand_reading(reading)["unknown_gpas"], {"4096": "not-on-lru"})

    def test_multiple_ram_regions_keep_high_gpas_and_absence_separate(self):
        with tempfile.TemporaryDirectory() as temporary:
            reading = self.reading(Path(temporary))
            high = copy.deepcopy(reading["regions"][0])
            high.update(slot=1, guest_base=1 << 32)
            reading["regions"].append(high)
            for key in ("observed_pages", "unknown_pages", "dirty_pages", "counted_pages"):
                reading[key] *= 2
            result = expand_reading(reading)
            self.assertEqual(result["accessed_gpas"], [0, 1 << 32])
            self.assertEqual(result["dirty_gpas"], [0, 8192, 1 << 32, (1 << 32) + 8192])
            self.assertEqual(result["unknown_gpas"],
                {"4096": "not-on-lru", str((1 << 32) + 4096): "not-on-lru"})
            self.assertNotIn(12288, result["dirty_gpas"])

    def test_old_batch_wrapper_does_not_bypass_native_partition_checks(self):
        with tempfile.TemporaryDirectory() as temporary:
            reading = self.reading(Path(temporary), access_absent=0)
            reading.update(schema="aries.tsb.tool-pages.llm-batch.v1", vm_generation="g")
            self.reject(reading, "cover RAM")

    def test_batch_to_comparison_preserves_native_validation(self):
        from batches import BatchTracker
        import compare
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary); reading = self.reading(root)
            class Observer:
                def arm(self, _): return {"arm_begin_ns": 1, "arm_end_ns": 2}
                def collect(self): return reading
            groups = []; tracker = BatchTracker(Observer(), groups.append)
            declared = {"session_id": "s", "turn_id": "1", "batch_id": "b", "llm_step_id": "b",
                        "expected_tool_call_ids": ["a"],
                        "requested_tools": [{"tool_call_id": "a", "tool_name": "exec"}]}
            member = {**declared, "tool_call_id": "a", "tool_name": "exec"}
            tracker.register(declared); tracker.begin(("s", "1", "a"), member)
            tracker.end(("s", "1", "a"), member)
            self.assertEqual(groups[-1]["reading"]["schema"], "aries.vmm-tracer.compact.v1")
            self.assertEqual(compare.summarize(groups, "group")["steps"][0]["counted_touched_pages"], 3)
            raw = bytes(8); (root / "access_absent.bin").write_bytes(raw)
            reading["regions"][0]["masks"]["access_absent"].update(
                pages=0, sha256=hashlib.sha256(raw).hexdigest())
            with self.assertRaisesRegex(ValueError, "cover RAM"):
                compare.summarize(groups, "group")


if __name__ == "__main__":
    unittest.main()
