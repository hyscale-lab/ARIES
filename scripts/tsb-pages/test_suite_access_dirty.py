import argparse,copy,json,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
import suite

class AccessDirtyPrepare(unittest.TestCase):
    def test_only_five_detailed_new_plugin_and_only_rstan_agent_override(self):
        with tempfile.TemporaryDirectory(dir=suite.REPO/".cache") as d:
            base=Path(d);config=base/"access.json";config.write_text("{}")
            attr=base/"attr.json";attr.write_text('{"enable_compression":false}')
            names=["fix-git","prove-plus-comm","overfull-hbox","rstan-to-pystan","schemelike-metacircular-eval","ordinary"]
            records=[{"task_id":name,"execution_task_id":name+"-001","shard_index":0,
                      "native_verifier_timeout_sec":1800,"native_agent_timeout_sec":1800} for name in names]
            args=argparse.Namespace(root=base/"run",shard_index=0,shard_count=1,selection_file=None,
                                   access_dirty_config=config,attribution_config=attr,port=18181,
                                   min_free_gib=20,owner="nehalem",observation_mode="group")
            pins={"terminalbench2":{"repository_url":"fixture","revision":"fixture"}}
            with patch.object(suite,"task_catalog",return_value=(pins,copy.deepcopy(records))),patch.object(suite,"source_record",return_value={}):
                suite.prepare(args)
            manifest=suite.read_json(args.root/"manifests/shard-0.json")
            for rec in manifest["tasks"]:
                profile=suite.read_json(rec["profile"])
                self.assertTrue(profile["harness"]["step_observation"]["plugin_dir"].endswith("tsb-access-dirty-plugin"))
                self.assertEqual(rec["detailed_attribution"],rec["task_id"]!="ordinary")
                self.assertEqual(profile["model"],suite.read_json(suite.TEMPLATE)["model"])
                self.assertEqual(profile["benchmark"]["tasks"],[rec["task_id"]])
                if rec["task_id"]=="rstan-to-pystan":
                    self.assertEqual(suite.read_json(profile["overrides_file"]),{"agent_timeout_seconds":5400})
                    self.assertEqual(rec["runtime_overrides"]["verifier_timeout_seconds"],1800)
                else:self.assertEqual(profile["overrides_file"],"")
            self.assertEqual(manifest["execution"]["automatic_paid_retries"],0)
            self.assertEqual(manifest["method"]["pause_policy"],"pause_during_llm_including_initial_request")
            self.assertEqual(manifest["selected_task_ids"],names)

if __name__=="__main__":unittest.main()
