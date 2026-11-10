import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
import suite

class ObserverAllowanceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.config = {"metadata_barrier_timeout_seconds":60,"publication_timeout_seconds":120,
                       "rstan_observer_allowance":dict(suite.RSTAN_OBSERVER_ALLOWANCE)}
        self.profile = {"harness":{"step_observation":{"plugin_dir":"original"}},
                        "resources":{"cpus":1,"memory_mb":8192},
                        "agent_timeout":5400,"verifier_timeout":1800}
    def tearDown(self): self.tmp.cleanup()
    def prepare(self):
        binding = suite.prepare_observer_override({"task_id":"rstan-to-pystan"},self.profile,self.config,self.root)
        return {"task_id":"rstan-to-pystan","observer_override":binding}
    def test_default_other_tasks_unchanged(self):
        old = copy.deepcopy(self.profile)
        for task,config in [("fix-git",self.config),("rstan-to-pystan",{})]:
            self.assertIsNone(suite.prepare_observer_override({"task_id":task},self.profile,config,self.root))
        self.assertEqual(self.profile,old)
        self.assertEqual(list(self.root.iterdir()),[])
    def test_scoped_hashes_and_native_limits(self):
        old = copy.deepcopy(self.profile)
        record = self.prepare()
        path = suite.validate_observer_override(record)
        config = json.loads(Path(path).read_text())
        self.assertEqual(config["metadata_barrier_timeout_seconds"],300)
        self.assertEqual(config["publication_timeout_seconds"],300)
        self.assertEqual(config["observer_http_timeout_seconds"],900)
        self.assertEqual(config["observer_hook_timeout_seconds"],905)
        self.profile["harness"] = old["harness"]
        self.assertEqual(self.profile,old)
        self.assertEqual(self.config["metadata_barrier_timeout_seconds"],60)
        self.assertEqual(self.config["publication_timeout_seconds"],120)
    def test_tampered_plugin_and_config_rejected(self):
        record=self.prepare()
        plugin=Path(record["observer_override"]["plugin_dir"])/"index.mjs"
        original=plugin.read_bytes()
        plugin.write_bytes(original+b"\n")
        with self.assertRaises(ValueError): suite.validate_observer_override(record)
        plugin.write_bytes(original)
        cfg=Path(record["observer_override"]["config"]["path"])
        cfg.write_text("{}")
        with self.assertRaises(ValueError): suite.validate_observer_override(record)
    def test_scope_and_unreviewed_allowance_rejected(self):
        record=self.prepare()
        record["task_id"]="fix-git"
        with self.assertRaises(ValueError): suite.validate_observer_override(record)
        with self.assertRaises(ValueError): suite.rstan_observer_allowance({"rstan_observer_allowance":{}})
    def test_native_registration_receives_exact_timeout(self):
        record=self.prepare()
        for directory,expected in [(suite.REPO/"scripts/tsb-access-dirty-plugin",125000),
                                    (Path(record["observer_override"]["plugin_dir"]),905000)]:
            import re
            source=(directory/"index.mjs").read_text()
            hooks=re.findall(r"timeoutMs:(\d+)",source)
            self.assertEqual(len(hooks),2)
            self.assertEqual({int(x) for x in hooks},{expected})
            self.assertIn("timeout:"+str(expected),source)

if __name__ == "__main__": unittest.main()
