"""Exact cardinality acceleration and separate postprocessor provenance."""
import contextlib
import hashlib
import io
import itertools
import json
import os
from pathlib import Path
import random
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import compare

HERE=Path(__file__).resolve().parent
REPO=HERE.parents[1]


def row(i,values,unknown=(),vm="vm",page=4096,valid=True):
    return {"event":"group_end","group_id":"llm-step-"+str(i),"llm_step_id":"llm-step-"+str(i),
            "session_id":"session","turn_id":"1","member_tool_call_ids":["call-"+str(i)],
            "reading":{"schema":"aries.tsb.tool-pages.llm-batch.v1","vm_instance":vm,
                       "page_bytes":page,"accessed_gpas":list(values),
                       "unknown_gpas":{str(g):"fixture" for g in unknown},"idle_gpas":[],
                       "arm_begin_ns":1,"arm_end_ns":2,"scan_begin_ns":4,"scan_end_ns":5},
            "measurement_valid":valid,"result":{"begin_monotonic_ns":2,"end_monotonic_ns":4}}


def reference_pairs(steps,field):
    result=[]
    for a,b in itertools.product(steps,repeat=2):
        reason=None
        if not a["measurement_valid"] or not b["measurement_valid"]:reason="invalid measurement window"
        elif not a["vm_instance"] or a["vm_instance"]!=b["vm_instance"]:reason="different or missing VM identity"
        elif a["page_bytes"]!=b["page_bytes"]:reason="different page granularity"
        elif a["record_kind"]!=b["record_kind"]:reason="different boundary kinds"
        metrics=compare.compare_sets(a[field],b[field]) if reason is None else {
            key:None for key in ("intersection_pages","union_pages","new_pages","lost_pages",
                                "jaccard","new_fraction_of_union","lost_fraction_of_union")}
        result.append({"from_index":a["index"],"to_index":b["index"],"comparable":reason is None,
                       "reason":reason,"shared_segment_ids":sorted(set(a["segment_ids"])&set(b["segment_ids"])),
                       **metrics})
    return result


class ExactComparisonTests(unittest.TestCase):
    def test_random_sparse_dense_and_unusual_addresses_match_set_algebra_in_both_directions(self):
        rng=random.Random(58471)
        for _ in range(80):
            rows=[]
            for i in range(rng.randrange(1,9)):
                page=rng.choice((4096,4096,8192))
                frames=rng.sample(range(1200),rng.randrange(0,120))
                if rng.random()<.5:frames += [(1<<75)+j for j in rng.sample(range(50),rng.randrange(0,10))]
                rng.shuffle(frames);split=rng.randrange(len(frames)+1)
                unknown=[g*page for g in frames[split:]]
                if rng.random()<.2:unknown.append(-page)
                rows.append(row(i,[g*page for g in frames[:split]],unknown,
                                rng.choice(("vm","vm","other",None)),page,rng.random()>.1))
            summary=compare.summarize(rows)
            self.assertEqual(summary["pairs"],reference_pairs(summary["steps"],"counted_touched_gpas"))
            self.assertEqual(summary["observed_only_pairs"],reference_pairs(summary["steps"],"accessed_gpas"))
            for original,normalized in zip(rows,summary["steps"]):
                reading=original["reading"]
                counted=sorted(set(reading["accessed_gpas"])|{int(g) for g in reading["unknown_gpas"]})
                self.assertEqual(normalized["counted_touched_gpas"],counted)
                self.assertEqual(normalized["counted_touched_bytes"],len(counted)*reading["page_bytes"])

    def test_huge_or_sparse_gpa_never_requires_huge_bitmap(self):
        for values in ([1<<120],[0,1<<120],[-4096,0],[3,4096],[0,12*1024**3]):
            prepared=compare._PreparedPages(values,4096)
            self.assertIsNotNone(prepared.sparse)
        mixed=[row(0,[0,4096]),row(1,[],[0,1<<120]),row(2,[],[-1,3])]
        summary=compare.summarize(mixed)
        self.assertEqual(summary["pairs"],reference_pairs(summary["steps"],"counted_touched_gpas"))
        self.assertIsNone(compare.compare_sets([],[])["jaccard"])


class CLISidecarTests(unittest.TestCase):
    def setUp(self):
        base=REPO/".cache/worker-stack"
        base.mkdir(parents=True,exist_ok=True)
        self.temporary=tempfile.TemporaryDirectory(dir=base)
        self.addCleanup(self.temporary.cleanup)
        self.root=Path(self.temporary.name)
        self.input=self.root/"groups.jsonl"
        self.rows=[row(0,[0,4096],[8192]),row(1,[4096,12288])]
        self.input.write_text("".join(json.dumps(r)+"\n" for r in self.rows))

    def test_cli_outputs_keep_schema_and_match_library_while_sidecar_is_separate(self):
        output=self.root/"comparison.json"
        result=subprocess.run([sys.executable,"-B",str(HERE/"compare.py"),"--input",str(self.input),
                               "--output",str(output),"--mode","group"],check=True,capture_output=True,text=True)
        expected=compare.summarize(self.rows,"group")
        expected["source"]={"path":str(self.input),"sha256":hashlib.sha256(self.input.read_bytes()).hexdigest()}
        self.assertEqual(output.read_text(),json.dumps(expected,indent=2)+"\n")
        self.assertEqual(output.with_suffix(".html").read_text(),compare.render(expected))
        self.assertEqual(set(json.loads(result.stdout)),{"records","json","html"})
        sidecar=json.loads((self.root/"comparison-postprocessor.json").read_text())
        self.assertEqual(sidecar["status"],"completed")
        self.assertEqual(sidecar["records"],2)
        self.assertEqual(sidecar["source_sha256"],hashlib.sha256((HERE/"compare.py").read_bytes()).hexdigest())
        self.assertEqual(sidecar["json_sha256"],hashlib.sha256(output.read_bytes()).hexdigest())
        self.assertEqual(sidecar["html_sha256"],hashlib.sha256(output.with_suffix(".html").read_bytes()).hexdigest())
        self.assertNotIn("postprocessor",expected)
        self.assertEqual((self.root/"comparison-postprocessor.json").stat().st_mode&0o777,0o600)

    def test_source_hash_is_captured_before_build_even_if_source_changes_during_it(self):
        source=self.root/"processor.py"
        source.write_bytes((HERE/"compare.py").read_bytes())
        before=hashlib.sha256(source.read_bytes()).hexdigest()
        original=compare.summarize
        output=self.root/"comparison.json"
        def build(rows,mode,artifact_root=None):
            initial=json.loads((self.root/"comparison-postprocessor.json").read_text())
            self.assertEqual(initial["status"],"started")
            self.assertEqual(initial["source_sha256"],before)
            source.write_text(source.read_text()+"\n# source changed after build began\n")
            return original(rows,mode,artifact_root=artifact_root)
        argv=["compare.py","--input",str(self.input),"--output",str(output),"--mode","group"]
        previous=os.umask(0o077);os.umask(previous)
        try:
            with patch.object(compare,"__file__",str(source)),patch.object(compare,"summarize",build),patch("sys.argv",argv),contextlib.redirect_stdout(io.StringIO()):
                compare.main()
        finally:os.umask(previous)
        sidecar=json.loads((self.root/"comparison-postprocessor.json").read_text())
        self.assertEqual(sidecar["source_sha256"],before)
        self.assertNotEqual(sidecar["source_sha256"],hashlib.sha256(source.read_bytes()).hexdigest())

    def test_failed_build_records_failure_and_preserves_exception(self):
        bad=row(0,[1])
        self.input.write_text(json.dumps(bad)+"\n")
        output=self.root/"comparison.json"
        result=subprocess.run([sys.executable,"-B",str(HERE/"compare.py"),"--input",str(self.input),
                               "--output",str(output)],capture_output=True,text=True)
        self.assertNotEqual(result.returncode,0)
        sidecar=json.loads((self.root/"comparison-postprocessor.json").read_text())
        self.assertEqual(sidecar["status"],"failed")
        self.assertEqual(sidecar["error_type"],"ValueError")
        self.assertFalse(output.exists())


if __name__=="__main__":unittest.main()
