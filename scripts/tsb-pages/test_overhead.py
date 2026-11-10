"""Retained, overlapping tracer work must not become invented latency savings."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("overhead.py")
spec = importlib.util.spec_from_file_location("tracer_overhead", SCRIPT)
overhead = importlib.util.module_from_spec(spec)
spec.loader.exec_module(overhead)


class OverheadTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.observer = Path(self.temporary.name) / "observer"
        self.root = self.observer / "access-dirty"
        self.root.mkdir(parents=True)

    def write(self, path, value, lines=False):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("".join(json.dumps(row) + "\n" for row in value) if lines else json.dumps(value))
        return path

    def test_nested_components_union_and_unattributed_time_do_not_double_count(self):
        self.write(self.root / "boundaries.jsonl", [
            {"kind": "arm", "begin_ns": 100, "end_ns": 200,
             "paused_begin_ns": 100, "paused_ack_ns": 110,
             "rotate_begin_ns": 120, "rotate_end_ns": 180,
             "publication_barrier_begin_ns": 140, "publication_barrier_end_ns": 160,
             "epoch_publication_begin_ns": 180, "epoch_publication_end_ns": 190,
             "api_journal_begin_ns": 190, "api_journal_end_ns": 195},
            {"kind": "collect", "begin_ns": 190, "end_ns": 240,
             "error": {"type": "TimeoutError"}, "censored_epoch": 4}], lines=True)
        report = overhead.summarize(self.observer)
        first = report["host_boundaries"]["records"][0]
        self.assertEqual(first["component_union_ns"], 85)
        self.assertEqual(first["unattributed_ns"], 15)
        self.assertEqual(report["host_boundaries"]["envelope_union_ns"], 100)
        self.assertEqual(report["host_boundaries"]["by_state"]["failed"]["envelope_union_ns"], 50)
        self.assertEqual(report["host_boundaries"]["components"]["publication_barrier"]["total"], 20)
        self.assertEqual(report["host_boundaries"]["by_kind"]["collect"]["failed"], 1)
        self.assertEqual(report["host_boundaries"]["by_kind"]["collect"]["censored"], 1)
        self.assertIsNone(report["host_boundaries"]["records"][1]["unattributed_ns"])

    def test_missing_timing_is_unavailable_and_sources_are_hashed(self):
        path = self.write(self.root / "boundaries.jsonl", [{"kind": "collect", "begin_ns": 10}], lines=True)
        report = overhead.summarize(self.observer)
        timing = report["host_boundaries"]["by_kind"]["collect"]["wall_ns"]
        self.assertEqual(timing["n"], 0)
        self.assertEqual(timing["missing"], 0)
        self.assertEqual(report["host_boundaries"]["by_state"]["incomplete"]["wall_ns"]["missing"], 1)
        self.assertIsNone(timing["total"])
        self.assertIsNone(report["native_epochs"]["metrics"]["dirty_wall_ns"]["median"])
        self.assertFalse(report["availability"]["host_api"])
        source = report["sources"]["access-dirty/boundaries.jsonl"]
        self.assertEqual(source["sha256"], hashlib.sha256(path.read_bytes()).hexdigest())
        self.assertEqual(source["bytes"], path.stat().st_size)
        self.assertNotIn("adjusted_latency", report)

    def epoch(self, directory, epoch, access, arm, *, baseline=False):
        metadata = {"epoch": epoch, "access": access,
                    "dirty": {"begin_monotonic_us": epoch * 10, "end_monotonic_us": epoch * 10 + 2},
                    "census": {"inventory_begin_ns": epoch * 100 + 5, "inventory_end_ns": epoch * 100 + 11,
                               "resident_population": {"resident_gpa_pages": 3}}}
        manifest = {"vm_generation": "generation", "record_id": "record-" + str(epoch), "complete": True,
                    "response": {"epoch": epoch, "next_epoch": epoch + 1, "arm_receipt": arm},
                    "metadata": {kind: copy.deepcopy(metadata) for kind in ("access", "dirty", "resident", "absent_ranges")}}
        self.write(self.root / directory / "manifest.json", manifest)
        self.write(self.root / directory / "phase.json", {"phase_kind": "tool", "group_id": "batch-1",
                   "censored": epoch == 3, "baseline_initialization": baseline, "excluded_from_workload": baseline})

    def test_vmm_native_costs_without_legacy_slot_files_deduplicate_shared_arm(self):
        first_arm = {"arm_begin_ns": 50, "arm_end_ns": 60, "arm_cpu_ns": 7,
                     "bitmap_write_begin_ns": 52, "bitmap_write_end_ns": 56}
        next_arm = {"arm_begin_ns": 200, "arm_end_ns": 220, "arm_cpu_ns": 8,
                    "bitmap_write_begin_ns": 202, "bitmap_write_end_ns": 208}
        self.epoch("initial-baseline", 1, None, first_arm, baseline=True)
        access = {**first_arm, "scan_begin_ns": 100, "scan_end_ns": 130, "scan_cpu_ns": 20,
                  "bitmap_read_begin_ns": 110, "bitmap_read_end_ns": 115}
        self.epoch("epoch-000002", 2, access, next_arm)
        self.epoch("epoch-000003", 3, {**next_arm, "scan_begin_ns": 300,
                   "scan_end_ns": 330, "scan_cpu_ns": 21}, None)
        report = overhead.summarize(self.observer)
        native = report["native_epochs"]
        self.assertEqual(native["metrics"]["idle_arm_wall_ns"]["n"], 2)
        self.assertEqual(native["metrics"]["idle_arm_wall_ns"]["total"], 30)
        self.assertEqual(native["metrics"]["idle_scan_wall_ns"]["n"], 1)
        self.assertEqual(native["metrics"]["dirty_wall_ns"]["total"], 4000)
        self.assertEqual(native["metrics"]["census_wall_ns"]["n"], 2)
        self.assertEqual(native["by_state"]["censored"]["metrics"]["idle_scan_wall_ns"]["n"], 1)
        self.assertEqual(native["by_phase"]["tool"]["censored"], 1)
        self.assertEqual(native["by_phase"]["initial_baseline"]["records"], 1)
        self.assertIn("idle_arm_cpu_ns", native["records"][-1]["reused_metrics"])
        self.assertEqual(report["status"], "available")

    def test_append_only_api_ledger_preferred_and_legacy_fallback_has_no_duplicates(self):
        calls = [{"endpoint": "/trace-epochs", "operation": "rotate", "begin_ns": 2, "end_ns": 7,
                  "response_bytes": 123, "outcome": "returned", "http_status": 200},
                 {"endpoint": "/metadata-capture", "operation": "export", "begin_ns": 8, "end_ns": 15,
                  "outcome": "failed", "http_status": 400}]
        legacy = self.write(self.root / "api-calls.json", calls)
        report = overhead.summarize(self.observer)
        self.assertEqual(report["api"]["host"]["format"], "legacy_json")
        self.assertEqual(report["api"]["host"]["wall_ns"]["total"], 5)
        self.assertEqual(report["api"]["host"]["by_state"]["failed"]["wall_ns"]["total"], 7)
        self.assertEqual(report["api"]["host"]["failed"], 1)
        self.assertEqual(report["api"]["host"]["by_state"]["failed"]["response_bytes"]["missing"], 1)
        self.write(self.root / "api-calls.jsonl", [{**calls[0], "sequence": 1}], lines=True)
        legacy.write_text("not JSON: the preferred ledger prevents parsing this obsolete snapshot")
        report = overhead.summarize(self.observer)
        self.assertEqual(report["api"]["host"]["records"], 1)
        self.assertNotIn("access-dirty/api-calls.json", report["sources"])
        self.assertEqual(report["status"], "available")
        self.write(self.root / "api-calls.jsonl", [{**calls[0], "sequence": 1}, {**calls[1], "sequence": 3}], lines=True)
        report = overhead.summarize(self.observer)
        self.assertEqual(report["status"], "partial")
        self.assertTrue(any("discontinuous" in row["error"] for row in report["errors"]))

    def test_detail_modes_keep_identity_scope_skips_and_child_phase_costs(self):
        for detail, closure in (("category", 32768), ("file", 446464)):
            directory = self.root / "captures" / detail
            metrics = {"schema": "aries.attribution-decode-metrics.v1", "attribution_detail": detail,
                "selected_pages": 96, "selected_gpas_sha256": "same-selection", "sample_pages": 0,
                "sample_gpas_sha256": "empty-sample", "kernel_sha256": "kernel", "layout_identity": "layout",
                "wall_ns": 100 if detail == "category" else 400, "cpu_ns": 90,
                "decoder_closure_pages": closure // 4096, "decoder_closure_bytes": closure,
                "provider_totals": {"read_requests": 7 if detail == "category" else 108,
                                    "requested_pages": 8, "cached_pages": 8, "consumed_pages": 8},
                "stages": {"page_categories": {"executed": True, "completed": True, "wall_ns": 20, "cpu_ns": 15},
                           "owner_resolution": {"executed": detail == "file", "completed": True,
                                                "wall_ns": 10 if detail == "file" else None, "cpu_ns": None}},
                "category_coverage": {"unresolved_pages": 0}, "scope": "synthetic frozen fixture"}
            self.write(directory / "attribution/decode-metrics.json", metrics)
            self.write(directory / "capture-begin.json", {"capture_id": detail, "vm_generation": "generation", "epoch": 3})
            calls = [{"endpoint": "/metadata-capture", "operation": "read_pages", "begin_ns": 1, "end_ns": 2,
                      "outcome": "returned", "response_bytes": 900}]
            self.write(directory / "api-calls.json", calls)
            self.write(directory / "companion-metrics.json", {"capture_id": detail, "mode": "attribute",
                "attribution_detail": detail, "api_calls": calls, "decode_metrics": metrics,
                "phases": {"capture_decode": {"completed": True, "wall_ns": 200, "cpu_ns": 100},
                           "publication": {"completed": detail != "file", "wall_ns": 300, "cpu_ns": 250}}})
        bootstrap = self.root / "captures/bootstrap"
        self.write(bootstrap / "companion-metrics.json", {"capture_id": "bootstrap", "mode": "bootstrap",
                   "attribution_detail": "category", "phases": {"bootstrap": {"completed": True, "wall_ns": 1000, "cpu_ns": 800}}})
        report = overhead.summarize(self.observer)
        groups = report["companion"]["by_detail"]
        self.assertEqual(groups["category"]["attribute"]["decoder_closure_bytes"]["median"], 32768)
        self.assertEqual(groups["category"]["bootstrap"]["phases"]["bootstrap"]["wall_ns"]["median"], 1000)
        self.assertEqual(groups["file"]["attribute"]["failed"], 1)
        skipped = groups["category"]["attribute"]["stages"]["owner_resolution"]
        self.assertEqual(skipped["executed_records"], 0)
        self.assertIsNone(skipped["wall_ns"]["total"])
        self.assertEqual(sum(row["records"] for row in report["api"]["children"]), 2)
        record = next(row for row in report["companion"]["records"] if row["capture_id"] == "category")
        self.assertEqual(record["selected_gpas_sha256"], "same-selection")
        self.assertEqual(record["layout_identity"], "layout")
        self.assertEqual(record["scope"], "synthetic frozen fixture")

    def test_plugin_intervals_require_same_pid_and_keep_failed_horizons(self):
        identity = {"plugin_pid": 7, "session_id": "session", "turn_id": "1", "batch_id": "batch"}
        path = self.write(self.observer / "plugin.jsonl", [
            {**identity, "event": "batch_requested", "plugin_monotonic_ns": "100"},
            {**identity, "event": "batch_confirmed", "plugin_monotonic_ns": "150"},
            {**identity, "event": "end_requested", "plugin_monotonic_ns": "200", "tool_call_id": "tool"},
            {**identity, "plugin_pid": 8, "event": "end_confirmed", "plugin_monotonic_ns": "250", "tool_call_id": "tool"},
            {**identity, "event": "measurement_error", "reason": "end_failed"},
            {**identity, "event": "terminal_seal", "valid": False}], lines=True)
        report = overhead.summarize(self.observer, path)
        self.assertEqual(report["plugin"]["by_kind"]["batch"]["median"], 50)
        self.assertNotIn("end", report["plugin"]["by_kind"])
        self.assertEqual(report["plugin"]["unconfirmed_requests"], 1)
        self.assertEqual(report["plugin"]["unmatched_confirmations"], 1)
        self.assertEqual(report["plugin"]["measurement_errors"], {"end_failed": 1})
        self.assertEqual(report["plugin"]["invalid_terminal_seals"], 1)

    def test_cost_headlines_exclude_failed_censored_attempts_but_keep_their_stats(self):
        events = []
        for index, (state, cost) in enumerate((("complete", 10), ("failed", 100), ("censored", 1000))):
            event = {"kind": "collect", "begin_ns": index * 10000, "end_ns": index * 10000 + cost}
            if state == "failed":
                event["error"] = {"message": "capture failed"}
            if state == "censored":
                event["censored_epoch"] = 3
            events.append(event)
            directory = self.root / "captures" / state
            self.write(directory / "companion-metrics.json", {"capture_id": state, "mode": "attribute",
                "attribution_detail": "category", "phases": {"capture_decode": {
                    "completed": state != "failed", "wall_ns": cost, "cpu_ns": cost}},
                "decode_metrics": {"attribution_detail": "category", "wall_ns": cost, "cpu_ns": cost,
                                   "selected_pages": 96, "decoder_closure_bytes": 32768}})
            if state == "censored":
                self.write(directory / "finalized-info.json", {"consistency": "discarded"})
        self.write(self.root / "boundaries.jsonl", events, lines=True)
        report = overhead.summarize(self.observer)
        host = report["host_boundaries"]["by_kind"]["collect"]
        companion = report["companion"]["by_detail"]["category"]["attribute"]
        for group in (host, companion):
            self.assertEqual(group["records"], 3)
            self.assertEqual(group["complete_records"], 1)
            self.assertEqual(group["wall_ns"]["median"], 10)
            self.assertEqual(group["by_state"]["failed"]["wall_ns"]["median"], 100)
            self.assertEqual(group["by_state"]["censored"]["wall_ns"]["median"], 1000)
        self.assertEqual(companion["cpu_ns"]["total"], 10)
        self.assertEqual(companion["by_state"]["failed"]["cpu_ns"]["total"], 100)
        page = overhead.render_html(report)
        self.assertIn("complete, noncensored only", page)
        self.assertIn("attempts by completion state", page)

    def test_native_dedup_does_not_let_an_earlier_censored_record_hide_success(self):
        arm = {"arm_begin_ns": 50, "arm_end_ns": 60, "arm_cpu_ns": 7}
        for epoch, (state, dirty_us) in enumerate((("censored", 50), ("failed", 500), ("complete", 5)), 1):
            directory = "epoch-%06d" % epoch
            self.epoch(directory, epoch, None, arm)
            path = self.root / directory / "manifest.json"
            manifest = json.loads(path.read_text())
            manifest["response"]["regions"] = [{"slot": 0, "guest_base": 0, "bytes": 16384, "page_count": 4}]
            for meta in manifest["metadata"].values():
                meta.update(vm_generation="generation", regions=manifest["response"]["regions"])
                meta["census"]["regions"] = meta["regions"]
                meta["dirty"]["end_monotonic_us"] = meta["dirty"]["begin_monotonic_us"] + dirty_us
            if state == "failed":
                manifest["error"] = {"message": "partial export"}
            self.write(path, manifest)
            self.write(path.with_name("phase.json"), {"phase_kind": "tool", "censored": state == "censored"})
        native = overhead.summarize(self.observer)["native_epochs"]
        self.assertEqual(native["metrics"]["idle_arm_wall_ns"]["n"], 1)
        self.assertEqual(native["metrics"]["idle_arm_wall_ns"]["total"], 10)
        self.assertEqual(native["metrics"]["idle_arm_cpu_ns"]["total"], 7)
        self.assertEqual(native["metrics"]["dirty_wall_ns"]["median"], 5000)
        self.assertEqual(native["by_state"]["failed"]["metrics"]["dirty_wall_ns"]["median"], 500000)
        self.assertEqual(native["by_state"]["censored"]["metrics"]["dirty_wall_ns"]["median"], 50000)
        self.assertEqual(native["records"][-1]["regions"], native["records"][-1]["census_regions"])

    def test_native_internal_identity_disagreement_is_retained_as_invalid(self):
        self.epoch("epoch-000001", 1, None, None)
        path = self.root / "epoch-000001/manifest.json"
        manifest = json.loads(path.read_text())
        manifest["response"]["vm_generation"] = "another-generation"
        for meta in manifest["metadata"].values():
            meta["epoch"] = 2
        self.write(path, manifest)
        report = overhead.summarize(self.observer)
        row = report["native_epochs"]["records"][0]
        self.assertTrue(row["invalid"])
        self.assertEqual(row["completion_state"], "failed")
        self.assertEqual(report["native_epochs"]["metrics"]["dirty_wall_ns"]["n"], 0)
        self.assertEqual(report["native_epochs"]["by_state"]["failed"]["metrics"]["dirty_wall_ns"]["n"], 1)
        self.assertEqual(report["status"], "partial")
        self.assertTrue(any("identity disagree" in error["error"] for error in report["errors"]))

    def test_cli_writes_json_html_with_unavailable_values_and_visible_errors(self):
        (self.root / "boundaries.jsonl").write_text("{truncated\n")
        output = self.observer / "report.json"
        subprocess.run([sys.executable, str(SCRIPT), "--observer", str(self.observer), "--output", str(output)], check=True)
        report = json.loads(output.read_text())
        self.assertEqual(report["status"], "partial")
        page = output.with_suffix(".html").read_text()
        self.assertIn("unavailable", page)
        self.assertIn("No adjusted workload latency", page)
        self.assertIn("boundaries.jsonl", page)
        self.assertIn("Input errors", page)


if __name__ == "__main__":
    unittest.main()
