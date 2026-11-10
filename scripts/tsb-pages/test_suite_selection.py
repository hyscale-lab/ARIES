import argparse
import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import suite


class SelectionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=suite.REPO / ".cache")
        self.base = Path(self.temp.name)
        self.records = [
            {"task_id": "a", "execution_task_id": "a-001", "shard_index": 0},
            {"task_id": "b", "execution_task_id": "b-001", "shard_index": 1},
            {"task_id": "c", "execution_task_id": "c-001", "shard_index": 0},
        ]
        self.selection = self.base / "selection.json"

    def tearDown(self):
        self.temp.cleanup()

    def write_selection(self, ids, reason="continue unattempted workloads"):
        self.selection.write_text(json.dumps({"task_ids": ids, "reason": reason}))
        return suite.selection_for(self.selection, self.records)

    def test_initial_catalog_and_selection_provenance(self):
        self.assertEqual(suite.selection_for(None, self.records)["task_ids"], ["a", "b", "c"])
        result = self.write_selection(["c"])
        self.assertEqual(result["source_sha256"], suite.digest(self.selection))
        self.assertEqual(result["source_path"], str(self.selection))

    def test_reject_unknown_duplicate_and_missing_reason(self):
        for ids, reason in [(["missing"], "x"), (["a", "a"], "x"), (["a"], "")]:
            with self.subTest(ids=ids, reason=reason), self.assertRaises(ValueError):
                self.write_selection(ids, reason)

    def test_reject_cross_worker_manifest_selection(self):
        with self.assertRaises(ValueError):
            suite.selected_records({"tasks": self.records, "shard_index": 0,
                                    "selected_task_ids": ["b"]})

    def test_fresh_subset_prepare_run_preserves_catalog_and_no_retry(self):
        self.write_selection(["c"])
        self.records += [{"task_id": task, "execution_task_id": task + "-001", "shard_index": 1}
                         for task in ("fix-git", "prove-plus-comm", "overfull-hbox", "rstan-to-pystan",
                                      "schemelike-metacircular-eval")]
        config = self.base / "config.json"
        config.write_text('{"backend":"vmm_owned"}')
        args = argparse.Namespace(root=self.base / "run", shard_index=0, shard_count=2,
                                  port=18181, min_free_gib=20, owner="nehalem",
                                  observation_mode="group", selection_file=self.selection,
                                  access_dirty_config=config, attribution_config=None)
        pins = {"terminalbench2": {"repository_url": "fixture", "revision": "fixture"}}
        def catalog(_):
            return pins, copy.deepcopy(self.records)
        with patch.object(suite, "task_catalog", side_effect=catalog), \
             patch.object(suite, "source_record", return_value={"fixture": True}), \
             patch.object(suite, "profile_for", return_value={"fixture": True, "harness": {"step_observation": {}}}):
            suite.prepare(args)
            manifest = suite.read_json(args.root / "manifests/shard-0.json")
            self.assertEqual(len(manifest["tasks"]), len(self.records))
            self.assertEqual(manifest["selected_task_ids"], ["c"])
            self.assertFalse((args.root / "a").exists())
            self.assertFalse((args.root / "b").exists())
            self.selection.unlink()  # Frozen manifest remains sufficient to execute.
            with patch.object(suite.os, "geteuid", return_value=0), \
                 patch.object(suite.Suite, "execute", lambda s: [r["task_id"] for r in s.tasks]):
                self.assertEqual(suite.run_suite(args), ["c"])
                status_path = args.root / "c/status.json"
                status = suite.read_json(status_path)
                status["paid_attempts"] = 1
                suite.atomic_json(status_path, status)
                with self.assertRaisesRegex(ValueError, "refusing a retry"):
                    suite.run_suite(args)


if __name__ == "__main__":
    unittest.main()
