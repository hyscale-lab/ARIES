import json
from pathlib import Path
import tempfile
import unittest
from suite_report import task_summary


class SuiteReportTests(unittest.TestCase):
    def setUp(self):
        self.temporary=tempfile.TemporaryDirectory();self.addCleanup(self.temporary.cleanup)
        self.root=Path(self.temporary.name)
        self.task={"task_id":"fixture","execution_task_id":"fixture-001","shard_index":0,
                   "native_environment":{"memory_mb":1}}
        self.directory=self.root/"fixture";self.observer=self.directory/"observer"
        self.observer.mkdir(parents=True)

    def write(self,path,value,rows=False):
        path.parent.mkdir(parents=True,exist_ok=True)
        path.write_text("".join(json.dumps(row)+"\n" for row in value) if rows else json.dumps(value))

    def comparison(self,**fields):
        step={"measurement_valid":True,"counted_touched_gpas":[0,4096,8192,12288],
              "vm_instance":"vm","page_bytes":4096,"counted_touched_bytes":16384,"accessed_bytes":4096,
              "unknown_pages":1,**fields}
        self.write(self.directory/"comparison.json",{"steps":[step],"pairs":[]})

    def native(self):
        regions=[{"slot":0,"guest_base":0,"bytes":65536,"page_count":16}]
        self.write(self.observer/"access-dirty/initial-arm.json",{
            "schema":"aries.firecracker.trace-epochs.v1","vm_generation":"g",
            "baseline_discarded":True,"regions":regions})
        self.write(self.observer/"groups.jsonl",[{"event":"group_end","measurement_valid":True,
            "reading":{"vm_generation":"g","regions":regions,
                       "access_absent_pages":15}}],rows=True)
        meta={"regions":regions,"census":{"resident_population":{"resident_gpa_pages":12},"regions":regions},
              "access":{"scan_begin_ns":100,"scan_end_ns":110},
              "dirty":{"begin_monotonic_us":3,"end_monotonic_us":5}}
        self.write(self.observer/"access-dirty/epoch-000002/manifest.json",{
            "vm_generation":"g","record_id":"record-2","complete":True,
            "response":{"epoch":2},"metadata":{"resident":meta}})
        self.write(self.observer/"access-dirty/epoch-000002/phase.json",{"phase_kind":"tool","censored":False})
        self.comparison(vm_generation="g",source_record_id="record-2",source_epoch=2)

    def test_native_residency_uses_census_without_legacy_slots_or_all_absent_mask(self):
        self.native()
        report=task_summary(self.root,self.task)
        self.assertFalse((self.observer/"selected-slots.json").exists())
        self.assertEqual(report["guest_memory_evidence"]["total_ram_bytes"],65536)
        self.assertEqual(report["guest_resident_at_scan_end_mib"]["median"],12*4096/1048576)
        self.assertEqual(report["counted_footprint_fraction_of_endpoint_resident_ram"]["median"],4/12)
        self.assertEqual(report["tracer_overhead"]["native_epochs"]["metrics"]["dirty_wall_ns"]["median"],2000)

    def test_missing_or_mismatched_native_evidence_is_unavailable(self):
        self.native()
        (self.observer/"access-dirty/epoch-000002/manifest.json").unlink()
        report=task_summary(self.root,self.task)
        self.assertIsNone(report["guest_resident_at_scan_end_mib"]["median"])
        self.assertIsNone(report["tracer_overhead"]["native_epochs"]["metrics"]["dirty_wall_ns"]["median"])
        groups=self.observer/"groups.jsonl"
        value=json.loads(groups.read_text());value["reading"]["vm_generation"]="wrong"
        self.write(groups,[value],rows=True)
        report=task_summary(self.root,self.task)
        self.assertTrue(report["guest_memory_evidence"]["errors"])
        self.assertIsNone(report["guest_memory_evidence"]["total_ram_bytes"])

    def test_stale_comparison_cannot_borrow_another_generation_or_epoch_census(self):
        self.native()
        self.comparison(vm_generation="g",source_record_id="record-2",source_epoch=3)
        report=task_summary(self.root,self.task)
        self.assertIsNone(report["guest_resident_at_scan_end_mib"]["median"])
        self.comparison(vm_generation="stale",source_record_id="record-2",source_epoch=2)
        report=task_summary(self.root,self.task)
        self.assertTrue(report["guest_memory_evidence"]["errors"])
        self.assertIsNone(report["guest_memory_evidence"]["total_ram_bytes"])

    def test_census_geometry_mismatch_cannot_pass_the_total_ram_bound(self):
        self.native()
        path=self.observer/"access-dirty/epoch-000002/manifest.json"
        manifest=json.loads(path.read_text())
        manifest["metadata"]["resident"]["census"]["regions"][0]["guest_base"]=1<<32
        self.write(path,manifest)
        report=task_summary(self.root,self.task)
        self.assertTrue(report["guest_memory_evidence"]["errors"])
        self.assertIsNone(report["guest_resident_at_scan_end_mib"]["median"])

    def test_legacy_footprint_fraction_keeps_dirty_contribution(self):
        self.write(self.observer/"selected-slots.json",[{"memory_size":65536}])
        self.comparison(not_resident_at_end_gpa_ranges=[[32768,65536]])
        report=task_summary(self.root,self.task)
        self.assertEqual(report["counted_footprint_fraction_of_endpoint_resident_ram"]["median"],16384/32768)
        self.assertEqual(report["tracer_overhead"]["status"],"no_measurements")

    def test_session_union_is_unavailable_across_vm_incarnations(self):
        self.comparison(vm_generation="first")
        path=self.directory/"comparison.json";comparison=json.loads(path.read_text())
        comparison["steps"].append({**comparison["steps"][0],"vm_generation":"second"})
        self.write(path,comparison)
        report=task_summary(self.root,self.task)
        self.assertFalse(report["session_sets_comparable"])
        self.assertIsNone(report["session_union_mib"])
        self.assertIsNone(report["session_common_mib"])


if __name__=="__main__":unittest.main()
