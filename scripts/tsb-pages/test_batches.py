"""LLM response membership, rather than native activity, defines the bitmap window."""
import unittest

import compare
from batches import BatchTracker


class FakeObserver:
    slots = [{"guest_phys_addr": 0, "memory_size": 65536}]

    def __init__(self):
        self.arms = []
        self.collections = 0
        self.armed = False

    def arm(self, name):
        if self.armed:
            raise AssertionError("double arm")
        self.armed = True
        self.arms.append(name)
        n = len(self.arms) * 100
        return {"arm_begin_ns": n, "arm_end_ns": n + 10,
                "bitmap_write_begin_ns": n + 3, "bitmap_write_end_ns": n + 8}

    def collect(self):
        if not self.armed:
            raise AssertionError("collect without arm")
        self.armed = False
        self.collections += 1
        n = len(self.arms) * 100
        return {
            "schema": "aries.tsb.tool-pages.idle.v1", "vm_instance": "same-vm",
            "vmm_pid": 123, "vmm_start_ticks": "456", "clock": "host-CLOCK_MONOTONIC",
            "page_bytes": 4096, "accessed_gpas": [0], "unknown_gpas": {"4096": "shared"},
            "idle_gpas": [8192], "arm_begin_ns": n, "arm_end_ns": n + 10,
            "scan_begin_ns": n + 60, "scan_end_ns": n + 90,
            "bitmap_read_begin_ns": n + 70, "bitmap_read_end_ns": n + 75,
            "arm_cpu_ns": 3, "scan_cpu_ns": 5,
            "not_resident_at_arm_gpa_ranges": [[40960, 45056]],
            "not_resident_at_end_gpa_ranges": [[49152, 53248]], "caveats": [],
        }


def declaration(batch="batch-1", calls=("a", "b")):
    return {
        "session_id": "session", "turn_id": "turn", "batch_id": batch, "llm_step_id": batch,
        "native_session_key": "native", "expected_tool_call_ids": list(calls),
        "requested_tools": [{"tool_call_id": call, "tool_name": "exec"} for call in calls],
        "native_assistant_message_id": "native-assistant-" + batch,
        "native_assistant_timestamp_ms": 1234,
    }


def key(call):
    return ("session", "turn", call)


def metadata(call, batch="batch-1", **extra):
    return {"session_id": "session", "turn_id": "turn", "batch_id": batch, "llm_step_id": batch,
            "native_session_key": "native", "tool_call_id": call, "tool_name": "exec", **extra}


class LLMBatches(unittest.TestCase):
    def fixture(self):
        observer, groups = FakeObserver(), []
        tracker = BatchTracker(observer, groups.append, wall_clock=lambda: 1_000_000)
        return tracker, observer, groups

    def test_sequential_calls_keep_one_arm_across_zero_active_gap(self):
        tracker, observer, groups = self.fixture()
        ack = tracker.register(declaration())
        self.assertEqual((ack["collection"], len(observer.arms)), ("armed", 1))
        tracker.begin(key("a"), metadata("a"))
        first = tracker.end(key("a"), metadata("a"), 140)
        self.assertFalse(tracker.active)
        self.assertIsNotNone(tracker.pending)
        self.assertEqual(first["observer_ack"]["collection"], "pending")
        self.assertEqual(first["observer_ack"]["remaining_expected_tool_call_ids"], ["b"])
        self.assertEqual(observer.collections, 0)
        tracker.begin(key("b"), metadata("b"))
        last = tracker.end(key("b"), metadata("b"), 170)
        self.assertEqual(last["observer_ack"]["collection"], "collected")
        self.assertEqual((len(observer.arms), observer.collections), (1, 1))
        self.assertIsNone(tracker.pending)
        self.assertEqual([r["event"] for r in groups], ["batch_begin", "group_end"])
        group = groups[-1]
        self.assertEqual(group["member_tool_call_ids"], ["a", "b"])
        self.assertEqual(group["completed_tool_call_ids"], ["a", "b"])
        self.assertEqual(group["concurrency_peak"], 1)
        self.assertEqual(group["reading"]["interior_bitmap_resets"], 0)
        self.assertEqual(group["reading"]["not_resident_at_arm_gpa_ranges"], [[40960, 45056]])
        self.assertEqual(group["reading"]["not_resident_at_end_gpa_ranges"], [[49152, 53248]])
        self.assertEqual(group["native_assistant_message_id"], "native-assistant-batch-1")
        summary = compare.summarize(groups, "group")
        self.assertEqual((summary["comparison_unit"], len(summary["steps"])), ("llm_reasoning_step", 1))
        self.assertEqual(summary["steps"][0]["record_kind"], "llm_reasoning_step")
        self.assertIn("ONE MATRIX ROW PER LLM REASONING STEP", compare.render(summary))

    def test_parallel_calls_share_same_batch_and_one_collected_ack(self):
        tracker, observer, groups = self.fixture()
        tracker.register(declaration())
        tracker.begin(key("a"), metadata("a"))
        tracker.begin(key("b"), metadata("b"))
        tracker.end(key("b"), metadata("b"), 140)
        tracker.end(key("a"), metadata("a"), 170)
        group = groups[-1]
        self.assertEqual((len(observer.arms), observer.collections), (1, 1))
        self.assertEqual(group["concurrency_peak"], 2)
        self.assertEqual(group["completed_tool_call_ids"], ["b", "a"])
        self.assertEqual(group["expected_tool_call_ids"], ["a", "b"])
        self.assertTrue(group["measurement_valid"])
        self.assertFalse(group["coverage_complete"])

    def test_all_immediate_error_results_need_no_fabricated_admission(self):
        tracker, observer, groups = self.fixture()
        tracker.register(declaration())
        a = tracker.end(key("a"), metadata("a", is_error=True), 140)
        b = tracker.end(key("b"), metadata("b", is_error=True), 170)
        self.assertEqual((len(observer.arms), observer.collections), (1, 1))
        self.assertFalse(a["begin_observed"])
        self.assertIsNone(a["result"]["begin_monotonic_ns"])
        self.assertFalse(b["begin_observed"])
        self.assertTrue(all(not m["begin_observed"] for m in groups[-1]["group_members"]))
        self.assertEqual(groups[-1]["concurrency_peak"], 0)
        self.assertTrue(groups[-1]["measurement_valid"])

    def test_late_foreign_or_duplicate_results_cannot_complete_current_batch(self):
        tracker, observer, groups = self.fixture()
        with self.assertRaisesRegex(ValueError, "no registered"):
            tracker.begin(key("a"), metadata("a"))
        tracker.register(declaration())
        with self.assertRaisesRegex(ValueError, "previous LLM batch"):
            tracker.register(declaration("other"))
        with self.assertRaisesRegex(ValueError, "another LLM batch"):
            tracker.end(key("a"), metadata("a", "other"), 140)
        with self.assertRaisesRegex(ValueError, "not requested"):
            tracker.end(key("unexpected"), metadata("unexpected"), 140)
        tracker.end(key("a"), metadata("a"), 140)
        with self.assertRaisesRegex(ValueError, "duplicate"):
            tracker.end(key("a"), metadata("a"), 150)
        self.assertEqual(observer.collections, 0)
        tracker.invalid = True
        tracker.end(key("b"), metadata("b"), 170)
        self.assertFalse(groups[-1]["measurement_valid"])

    def test_call_ids_can_be_reused_in_later_llm_batches(self):
        tracker, observer, groups = self.fixture()
        for batch in ("first", "second"):
            tracker.register(declaration(batch))
            tracker.end(key("a"), metadata("a", batch), 140)
            tracker.end(key("b"), metadata("b", batch), 170)
        complete = [row for row in groups if row["event"] == "group_end"]
        self.assertEqual((len(observer.arms), observer.collections), (2, 2))
        self.assertEqual([row["llm_step_id"] for row in complete], ["first", "second"])
        self.assertEqual([row["expected_tool_call_ids"] for row in complete], [["a", "b"], ["a", "b"]])
        summary = compare.summarize(groups)
        self.assertEqual((summary["comparison_unit"], len(summary["steps"]), len(summary["pairs"])),
                         ("llm_reasoning_step", 2, 4))

    def test_historical_busy_period_reading_is_not_reinterpreted_as_llm_step(self):
        tracker, _, groups = self.fixture()
        tracker.register(declaration())
        tracker.end(key("a"), metadata("a"), 140)
        tracker.end(key("b"), metadata("b"), 170)
        historical = dict(groups[-1])
        historical["reading"] = dict(historical["reading"], schema="aries.tsb.tool-pages.busy-period.v1")
        historical["reading"].pop("batch_id")
        historical["reading"].pop("llm_step_id")
        summary = compare.summarize([historical])
        self.assertEqual(summary["comparison_unit"], "busy_period_group")
        self.assertIn("HISTORICAL BUSY-PERIOD PROTOTYPE", compare.render(summary))
        mixed = compare.summarize([historical, groups[-1]])
        self.assertEqual(mixed["comparison_unit"], "mixed_group_schemas")
        self.assertFalse(mixed["pairs"][1]["comparable"])

    def test_expected_id_list_order_must_match_native_declaration(self):
        tracker, observer, _ = self.fixture()
        body = declaration()
        body["expected_tool_call_ids"] = ["b", "a"]
        with self.assertRaisesRegex(ValueError, "IDs/order"):
            tracker.register(body)
        self.assertFalse(observer.arms)

    def test_different_tool_cannot_complete_a_declared_native_call(self):
        for endpoint in ("begin","end"):
            with self.subTest(endpoint=endpoint):
                tracker,observer,groups=self.fixture()
                tracker.register(declaration(calls=("a",)))
                with self.assertRaisesRegex(ValueError,"tool name"):
                    getattr(tracker,endpoint)(key("a"),metadata("a",tool_name="read"))
                self.assertEqual(observer.collections,0)
                self.assertEqual([row["event"] for row in groups],["batch_begin"])
                self.assertFalse(tracker.pending["completed"])


if __name__ == "__main__":
    unittest.main()
