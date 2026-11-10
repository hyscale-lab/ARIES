import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from paused import PausedAttributionObserver

class Base:
    pid=1; vm_id="fixture"; slots=[]; start="fixture-start"
    def __init__(self, calls): self.calls=calls
    def identity(self): pass
    def arm(self,name): self.calls.append("arm"); return {"arm_end_ns":1}
    def collect(self): self.calls.append("collect"); return {"accessed_gpas":[0],"unknown_gpas":{}}
    def close(self): self.calls.append("close")

class Decoder:
    def __init__(self,calls,fail=None): self.calls=calls;self.fail=fail
    def bootstrap(self):
        self.calls.append("bootstrap")
        if self.fail=="bootstrap": raise ValueError("bad layout")
        return {"ok":True}
    def capture(self,reading,output):
        self.calls.append("capture")
        if self.fail=="capture": raise ValueError("bad capture")
        return {"pages":len(reading["accessed_gpas"])}
    def close(self): self.calls.append("decoder-close")

class Boundaries(unittest.TestCase):
    def make(self,directory,calls,fail=None):
        result=PausedAttributionObserver(Base(calls),{"socket_path":"/fixture"},Path(directory)/"capture",lambda *a: Decoder(calls,fail))
        result._state=lambda state:calls.append(state)
        return result
    def test_bootstrap_before_arm_and_capture_after_finalized_bitmap(self):
        with tempfile.TemporaryDirectory() as d:
            calls=[];o=self.make(d,calls)
            o.arm("first");r=o.collect();o.arm("second");o.collect();o.close()
            self.assertEqual(calls,["Paused","bootstrap","arm","Resumed","Paused","collect","capture","Resumed","Paused","arm","Resumed","Paused","collect","capture","Resumed","decoder-close","close"])
            self.assertIn("resume_ack_ns",r["boundary_pause"])
            self.assertEqual(r["attribution"],{"pages":1})
    def test_bootstrap_failure_resumes_without_arming(self):
        with tempfile.TemporaryDirectory() as d:
            calls=[];o=self.make(d,calls,"bootstrap")
            with self.assertRaisesRegex(ValueError,"bad layout"):o.arm("first")
            self.assertEqual(calls,["Paused","bootstrap","Resumed"])
            events=[json.loads(x) for x in (o.output/"boundaries.jsonl").read_text().splitlines()]
            self.assertEqual(events[0]["error"]["message"],"bad layout")
            o.close()
    def test_capture_failure_retains_bitmap_and_resumes(self):
        with tempfile.TemporaryDirectory() as d:
            calls=[];o=self.make(d,calls,"capture")
            o.arm("first")
            with self.assertRaisesRegex(ValueError,"bad capture"):o.collect()
            self.assertEqual(calls[-3:],["collect","capture","Resumed"])
            self.assertEqual(json.loads((o.output/"step-000001/reading.json").read_text())["accessed_gpas"],[0])
            o.close()
    def test_uncertain_pause_failure_still_attempts_resume(self):
        with tempfile.TemporaryDirectory() as d:
            calls=[];o=self.make(d,calls)
            def state(s):
                calls.append(s)
                if s=="Paused":raise TimeoutError("uncertain")
            o._state=state
            with self.assertRaises(TimeoutError):o.arm("first")
            self.assertEqual(calls,["Paused","Resumed"])
            o.close()
    def test_resume_failure_is_recorded_and_close_retries(self):
        with tempfile.TemporaryDirectory() as d:
            calls=[];o=self.make(d,calls)
            def state(s):
                calls.append(s)
                if s=="Resumed":raise TimeoutError("resume failed")
            o._state=state
            with self.assertRaises(TimeoutError):o.arm("first")
            self.assertTrue(o.paused)
            events=[json.loads(x) for x in (o.output/"boundaries.jsonl").read_text().splitlines()]
            self.assertIn("resume_error",events[0])
            o._state=lambda s:calls.append(s)
            o.close()
            self.assertEqual(calls[-3:],["Resumed","decoder-close","close"])

if __name__=="__main__":unittest.main()
