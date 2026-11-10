import argparse,copy,json,tempfile,unittest
from pathlib import Path
from unittest.mock import patch
import suite

class AgentTimeoutMap(unittest.TestCase):
    def fixture(self, base, mapping, selected=None):
        names=["fix-git","prove-plus-comm","overfull-hbox","rstan-to-pystan","schemelike-metacircular-eval","ordinary"]
        records=[{"task_id":n,"execution_task_id":n+"-001","shard_index":0,
                  "native_verifier_timeout_sec":2400 if n=="schemelike-metacircular-eval" else 1800,
                  "native_agent_timeout_sec":2400 if n=="schemelike-metacircular-eval" else 1800,
                  "native_cpu_count":2,"native_memory_mb":8192} for n in names]
        config=base/"access.json";config.write_text(json.dumps({"agent_timeout_overrides":mapping}))
        attr=base/"attr.json";attr.write_text('{"enable_compression":false}')
        selection=None
        if selected is not None:
            selection=base/"selection.json";selection.write_text(json.dumps({"task_ids":selected,"reason":"fixture selected reruns"}))
        args=argparse.Namespace(root=base/"run",shard_index=0,shard_count=1,selection_file=selection,
            access_dirty_config=config,attribution_config=attr,port=18181,min_free_gib=20,owner="nehalem",observation_mode="group")
        pins={"terminalbench2":{"repository_url":"fixture","revision":"fixture"}}
        return args,pins,records

    def test_two_explicit_caps_bind_profiles_and_preserve_native_verifier_resources(self):
        with tempfile.TemporaryDirectory(dir=suite.REPO/".cache") as d:
            caps={"schemelike-metacircular-eval":9600,"rstan-to-pystan":21600}
            args,pins,records=self.fixture(Path(d),caps,list(caps))
            with patch.object(suite,"task_catalog",side_effect=lambda count:(pins,copy.deepcopy(records))),patch.object(suite,"source_record",return_value={}):
                suite.prepare(args)
                manifest=suite.read_json(args.root/"manifests/shard-0.json")
                self.assertEqual(manifest["budget"]["agent_timeout_overrides"],caps)
                self.assertEqual(manifest["execution"]["attempts_per_task"],1)
                self.assertEqual(manifest["execution"]["automatic_paid_retries"],0)
                for r in manifest["tasks"]:
                    original=next(x for x in records if x["task_id"]==r["task_id"])
                    for k in original:self.assertEqual(r[k],original[k])
                    if r["task_id"] not in caps:
                        self.assertNotIn("runtime_overrides",r);continue
                    profile=suite.read_json(r["profile"])
                    self.assertEqual(suite.read_json(profile["overrides_file"]),{"agent_timeout_seconds":caps[r["task_id"]]})
                    self.assertEqual(r["runtime_overrides"]["sha256"],suite.digest(profile["overrides_file"]))
                    self.assertEqual(r["runtime_overrides"]["verifier_timeout_seconds"],original["native_verifier_timeout_sec"])
                    self.assertEqual(profile["model"],suite.read_json(suite.TEMPLATE)["model"])
                with patch.object(suite.os,"geteuid",return_value=0),patch.object(suite.Suite,"execute",return_value=True):
                    self.assertTrue(suite.run_suite(args))
                    target=Path(next(r for r in manifest["tasks"] if r["task_id"]=="rstan-to-pystan")["runtime_overrides"]["path"])
                    target.write_text('{"agent_timeout_seconds":1}')
                    with self.assertRaisesRegex(ValueError,"runtime timeout override changed"):
                        suite.run_suite(args)

    def test_explicit_empty_map_restores_native_agent_limits(self):
        with tempfile.TemporaryDirectory(dir=suite.REPO/".cache") as d:
            args,pins,records=self.fixture(Path(d),{})
            with patch.object(suite,"task_catalog",return_value=(pins,copy.deepcopy(records))),patch.object(suite,"source_record",return_value={}):
                suite.prepare(args)
            m=suite.read_json(args.root/"manifests/shard-0.json")
            self.assertEqual(m["budget"]["agent_timeout_overrides"],{})
            for r in m["tasks"]:
                self.assertNotIn("runtime_overrides",r)
                self.assertEqual(suite.read_json(r["profile"])["overrides_file"],"")

    def test_bad_keys_and_nonpositive_or_noninteger_caps_rejected_before_profiles(self):
        for caps in (None,[],{"missing":9600},{"ordinary":0},{"ordinary":-1},{"ordinary":True},{"ordinary":1.5},{"ordinary":"9600"}):
            with self.subTest(caps=caps),tempfile.TemporaryDirectory(dir=suite.REPO/".cache") as d:
                args,pins,records=self.fixture(Path(d),caps)
                with patch.object(suite,"task_catalog",return_value=(pins,copy.deepcopy(records))),patch.object(suite,"source_record",return_value={}):
                    with self.assertRaisesRegex(ValueError,"positive integer"):
                        suite.prepare(args)
                self.assertFalse(list(args.root.glob("*/profile.json")))

if __name__=="__main__":unittest.main()
