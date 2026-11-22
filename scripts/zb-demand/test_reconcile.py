import tempfile
import unittest
from dataclasses import replace
from pathlib import Path

from reconcile import Fault, parse_faults, reconcile


example_fault = Fault(
    begin_ns=170, end_ns=180, pid=123, tid=124, vcpu=0, address=4182,
    gfn=1, tdp=1, prefetch=0, error_code=20, ret=4,
    hva=16384, lookups=2, hva_conflicts=0, lookup_ns=8,
    mm_faults=3, major_flags=1, file_faults=2, file_major=1, swap_faults=0,
    swap_major=0, cow_calls=0, major_accounted=1, minor_accounted=0, io_wait_ns=3,
)


class ReconcileTests(unittest.TestCase):
    def test_tool_demand_joins_exact_registered_address_and_collector_clock(self):
        setup = {
            "status": "ok", "phase": "pre-tool", "step": 9,
            "freeze": {"pause_request_ns": 125, "resume_ack_ns": 133},
        }
        capture = {
            "status": "ok", "phase": "post-tool", "step": 9, "from_step": 9,
            "window_kind": "tool", "vmm_pid": 123, "vmm_start_ticks": "455",
            "ram_slots": [{"guest_phys_addr": 0, "memory_size": 20480, "userspace_addr": 12288}],
            "window": {
                "arm_begin_ns": 129, "arm_end_ns": 132, "scan_begin_ns": 220, "scan_end_ns": 230,
                "not_resident_at_arm_gpa_ranges": [[4096, 12288]],
                "not_resident_at_end_gpa_ranges": [[8192, 12288]],
            },
        }
        steps = [{"event": event, "monotonic_ns": at, "step_id": 9, "rollout_line": 15}
                 for event, at in (("source_event", 120), ("tool_dispatch", 145), ("tool_reply", 208))]
        faults = [
            example_fault,
            replace(example_fault, pid=432, begin_ns=182, end_ns=192),
            replace(example_fault, begin_ns=185, end_ns=195, hva_conflicts=1),
            replace(example_fault, begin_ns=200, end_ns=211, hva=32768),
        ]
        result = reconcile(50, 240, faults, steps, [setup, capture])
        self.assertEqual(result["restored_vmm_pid"], 123)
        self.assertEqual(result["non_vmm_faults_excluded"], 1)
        self.assertEqual(result["coverage"], {
            "matched-absent-at-arm": 1, "path:filemap-major-path": 1, "scope:tool": 1,
            "absent-tdp-without-consistent-hva-proof": 1, "absent-tdp-hva-mismatch": 1,
        })
        event, = result["matches"]
        self.assertEqual(event["gpa_page"], 4096)
        self.assertFalse(event["absent_at_end"])
        self.assertTrue(event["first_match_for_page_in_window"])
        self.assertEqual(event["dispatch_to_fault_ns"], 25)
        self.assertEqual(event["source_boundary_to_fault_minus_max_freeze_ns"], 42)
        self.assertEqual(event["lookup_in_iowait_offcpu_ns"], 3)

    def test_unpaired_or_dropped_bpf_trace_fails_closed(self):
        with tempfile.TemporaryDirectory() as root:
            events = Path(root) / "events"
            errors = Path(root) / "stderr"
            events.write_text("ready 10\nend 20 0\n")
            errors.write_text("Lost 12 events\n")
            with self.assertRaisesRegex(ValueError, "loss check"):
                parse_faults(events, errors)
            errors.write_text("")
            self.assertEqual(parse_faults(events, errors), (10, 20, []))
            events.write_text("ready 10\nend 20 1\n")
            with self.assertRaisesRegex(ValueError, "shutdown"):
                parse_faults(events, errors)


if __name__ == "__main__":
    unittest.main()
