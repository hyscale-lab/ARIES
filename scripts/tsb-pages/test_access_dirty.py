import base64
import gzip
import struct
import json
from pathlib import Path
import tempfile
import threading
import time
import unittest
from access_dirty import AccessDirtyObserver, pack_pages, validate_dirty

SLOTS = [{"guest_phys_addr": 0, "memory_size": 16384, "userspace_addr": 65536}]
def response(epoch, next_epoch=None, bits=0):
    raw = base64.b64encode(bits.to_bytes(8, "little")).decode()
    zero = base64.b64encode(bytes(8)).decode()
    return {"schema": "aries.firecracker.dirty.v1", "epoch": epoch, "next_epoch": next_epoch,
            "page_bytes": 4096, "active_epoch": next_epoch, "regions": [{
                "slot": 0, "guest_base": 0, "bytes": 16384, "page_count": 4,
                "kvm_dirty_b64": raw, "vmm_dirty_b64": zero, "combined_dirty_b64": raw,
                "kvm_dirty_pages": bits.bit_count(), "vmm_dirty_pages": 0,
                "combined_dirty_pages": bits.bit_count()}]}

class Base:
    pid=1; vm_id="vm"; start="start"; slots=SLOTS
    def __init__(self,calls):self.calls=calls;self.armed=False;self.clock=0
    def identity(self):pass
    def inventory(self):return {0:1,4096:2,8192:3,12288:4},[]
    def arm(self,name):
        if self.armed:raise ValueError("double arm")
        self.armed=True;self.clock+=1;self.calls.append("access-arm")
        return {"arm_begin_ns":self.clock,"arm_end_ns":self.clock,"resident_pages_at_arm":4}
    def collect(self):
        if not self.armed:raise ValueError("unarmed")
        self.armed=False;self.calls.append("access-collect")
        return {"accessed_gpas":[0],"idle_gpas":[4096],"unknown_gpas":{"8192":"shared"},
                "page_bytes":4096,"vm_instance":"vm","vmm_pid":1,"vmm_start_ticks":"start"}
    def close(self):self.calls.append("base-close")

class Dirty:
    def __init__(self,calls,changed=False):self.calls=calls;self.requests=[];self.changed=changed
    def request(self,op,epoch,next_epoch=None):
        self.calls.append("dirty-"+op);self.requests.append((op,epoch,next_epoch))
        r=response(epoch,next_epoch,2 if self.changed and epoch==3 else 8)
        if op=="arm":r.update(active_epoch=1,boot_baseline_discarded=True,vcpu_paused=True)
        return r

class Decoder:
    def __init__(self,calls,gate=None):self.calls=calls;self.gate=gate;self.started=threading.Event()
    def bootstrap(self):return {"anchors":"fixture"}
    def capture_metadata(self,reading):
        self.calls.append("metadata-read");self.metadata_read_gpas.add(4096);self.started.set()
        if self.gate:self.gate.wait(5)
        return {"metadata":{4096:bytes(4096)}}
    def publish_capture(self,data,output):
        self.calls.append("metadata-publish");Path(output).mkdir(parents=True,exist_ok=True)
        return {"metadata_pages":1}
    def close(self):self.calls.append("decoder-close")

class Epochs(unittest.TestCase):
    def make(self,d,decoder=None,changed=False):
        calls=decoder.calls if decoder else []
        client=Dirty(calls,changed)
        o=AccessDirtyObserver(Base(calls),{"socket_path":"/fixture"},Path(d)/"capture",client,decoder)
        o._state=lambda state:calls.append(state)
        return o,calls,client
    def test_stuck_metadata_reader_exits_process_before_any_resume(self):
        import subprocess, sys
        with tempfile.TemporaryDirectory() as d:
            program = """
import io, threading, types
from pathlib import Path
from access_dirty import AccessDirtyObserver
o=AccessDirtyObserver.__new__(AccessDirtyObserver)
o.config={"metadata_abort_grace_seconds":0.02}
o.pid=123;o.start=456;o.vm_id="fixture";o.socket_path="/fixture"
o.output=Path(OUTPUT);o.events=io.StringIO();o.phases=io.StringIO()
o.decoder=types.SimpleNamespace(metadata_read_gpas={4096})
o._cancel_reader({"metadata_abort":threading.Event(),"metadata_reads_done":threading.Event()})
raise RuntimeError("unsafe return")
""".replace("OUTPUT", repr(d))
            result=subprocess.run([sys.executable,"-c",program],timeout=5,
                                  cwd=Path(__file__).resolve().parent)
            self.assertEqual(result.returncode,86)
            receipt=json.loads((Path(d)/"fatal-reader.json").read_text())
            self.assertFalse(receipt["measurement_valid"])
            self.assertEqual(receipt["partial_metadata_read_gpas"],[4096])
            self.assertGreaterEqual(receipt["fatal_ns"],receipt["cancel_begin_ns"])

    def test_initial_pause_native_tool_resume_gap_hold_and_terminal_release(self):
        with tempfile.TemporaryDirectory() as d:
            o,calls,dirty=self.make(d)
            o.session_start();self.assertTrue(o.paused)
            self.assertTrue(json.loads((o.output/"initial-dirty-arm.json").read_text())["boot_baseline_discarded"])
            o.arm("one");self.assertFalse(o.paused)
            r=o.collect();self.assertTrue(o.paused)
            o.arm("two");self.assertFalse(o.paused)
            o.collect();o.session_end();self.assertFalse(o.paused);o.close()
            self.assertEqual(dirty.requests,[("arm",1,None),("collect",1,2),("collect",2,3),
                                            ("collect",3,4),("collect",4,5),("collect",5,None)])
            phases=[json.loads(l) for l in (o.output/"phases.jsonl").read_text().splitlines()]
            self.assertEqual([p["phase_kind"] for p in phases],["LLM_wait","tool","LLM_wait","tool","LLM_wait"])
            self.assertTrue(phases[-1]["censored"])
            masks=r["regions"][0]["masks"]
            self.assertEqual(masks["effective_touched"]["pages"],3)
            self.assertEqual(masks["dirty_outside_idle_counted"]["pages"],1)
            self.assertEqual(r["observed_pages"],1)
            from compact import expand_reading
            expanded=expand_reading(r)
            self.assertEqual(expanded["accessed_gpas"],[0])
            self.assertEqual(expanded["unknown_gpas"],{"8192":"shared"})
            self.assertEqual(expanded["dirty_gpas"],[12288])
            import shutil
            relocated=Path(d)/"relocated"
            shutil.copytree(r["artifact_directory"], relocated)
            relocated_reading=dict(r, artifact_directory="/missing/disposable-worker")
            replay=expand_reading(relocated_reading, artifact_directory=relocated)
            self.assertEqual(replay["dirty_gpas"], expanded["dirty_gpas"])
            self.assertEqual(replay["artifact_directory"], "/missing/disposable-worker")
    def test_async_metadata_delivers_result_then_barriers_next_tool_and_checks_dirty(self):
        with tempfile.TemporaryDirectory() as d:
            calls=[];gate=threading.Event();decoder=Decoder(calls,gate);o,_,_=self.make(d,decoder,True)
            o.session_start();o.arm("one")
            r=o.collect();self.assertTrue(r["attribution_pending"])
            self.assertTrue(decoder.started.wait(1));self.assertTrue(o.paused)
            completed=threading.Event()
            thread=threading.Thread(target=lambda:(o.arm("two"),completed.set()));thread.start()
            self.assertFalse(completed.wait(.05));gate.set();thread.join(3);self.assertTrue(completed.is_set())
            self.assertFalse(o.paused)
            check=json.loads((o.output/"epoch-000002-tool/attribution-consistency.json").read_text())
            self.assertFalse(check["valid"]);self.assertEqual(check["changed_metadata_gpas"],[4096])
            gap=json.loads((o.output/"epoch-000003-LLM_wait/phase.json").read_text())
            self.assertEqual(gap["regions"][0]["masks"]["observer_metadata_read"]["pages"],1)
            self.assertFalse(o.invalid,"attribution inconsistency must not discard later raw epochs")
            o.session_end();o.close()
    def test_no_dirty_metadata_intersection_is_qualified_valid(self):
        with tempfile.TemporaryDirectory() as d:
            calls=[];o,_,_=self.make(d,Decoder(calls))
            o.session_start();o.arm("one");o.collect();o.session_end();o.close()
            check=json.loads((o.output/"epoch-000002-tool/attribution-consistency.json").read_text())
            self.assertTrue(check["valid"]);self.assertIn("covered writers",check["scope"])
    def test_uncertain_pause_and_dirty_failure_always_resume(self):
        with tempfile.TemporaryDirectory() as d:
            o,calls,_=self.make(d)
            def state(s):
                calls.append(s)
                if s=="Paused":raise TimeoutError("uncertain")
            o._state=state
            with self.assertRaises(TimeoutError):o.session_start()
            self.assertEqual(calls[-1],"Resumed");o.close()
        with tempfile.TemporaryDirectory() as d:
            o,calls,dirty=self.make(d);o.session_start()
            dirty.request=lambda *a: (_ for _ in ()).throw(ValueError("dirty failure"))
            with self.assertRaises(ValueError):o.arm("one")
            self.assertEqual(calls[-1],"Resumed");self.assertTrue(o.invalid);o.close()
    def test_missing_initial_pause_is_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            o,_,_=self.make(d)
            with self.assertRaisesRegex(RuntimeError,"initial session"):o.arm("one")
            o.close()
    def test_timeout_cancels_live_reader_before_resume_and_poisoned_next_window(self):
        class Cancellable(Decoder):
            def capture_metadata(self, reading):
                self.metadata_read_gpas.add(4096);self.started.set()
                self.capture_abort.wait(2)
                self.calls.append("reader-stopped")
                raise RuntimeError("canceled")
        with tempfile.TemporaryDirectory() as d:
            calls=[];decoder=Cancellable(calls);o,_,_=self.make(d,decoder)
            o.config["metadata_barrier_timeout_seconds"]=.01
            o.session_start();o.arm("one");o.collect()
            self.assertTrue(decoder.started.wait(1))
            with self.assertRaisesRegex(RuntimeError,"metadata barrier"):o.arm("two")
            self.assertTrue(o.invalid);self.assertFalse(o.paused)
            self.assertLess(calls.index("reader-stopped"),len(calls)-1)
            self.assertEqual(calls[-1],"Resumed")
            gap=json.loads((o.output/"epoch-000003-LLM_wait/phase.json").read_text())
            self.assertEqual(gap["regions"][0]["masks"]["observer_metadata_read"]["pages"],1)
            o.close()
    def test_clean_terminal_gap_is_completed_not_censored(self):
        with tempfile.TemporaryDirectory() as d:
            o,_,_=self.make(d);o.session_start();o.session_end("before_agent_finalize",True);o.close()
            phase=json.loads((o.output/"epoch-000001-LLM_wait/phase.json").read_text())
            self.assertFalse(phase["censored"])
    def test_census_aliases_and_resident_zero_remain_reconstructable(self):
        with tempfile.TemporaryDirectory() as d:
            o,_,_=self.make(d)
            o.base.inventory=lambda:({0:11,4096:11,8192:0},[[12288,16384]])
            o.session_start();o.session_end("before_agent_finalize",True);o.close()
            path=o.output/"epoch-000001-LLM_wait"
            r=json.loads((path/"phase.json").read_text())
            self.assertEqual(r["resident_at_collect"]["distinct_nonzero_host_pfns"],1)
            self.assertEqual(r["resident_at_collect"]["zero_host_pfn_pages"],1)
            raw=gzip.decompress((path/r["regions"][0]["host_pfn_maps"]["collect"]["file"]).read_bytes())
            self.assertEqual(struct.unpack("<4Q",raw),(11,11,0,0))
            mask=(path/r["regions"][0]["masks"]["resident_collect"]["file"]).read_bytes()
            self.assertEqual(int.from_bytes(mask,"little"),7)
    def test_dirty_only_effective_pages_are_in_comparison_even_without_unknown(self):
        import compare
        from test_compare import row
        a=row([0]);a["reading"]["dirty_gpas"]=[4096]
        b=row([4096]);b["reading"]["dirty_gpas"]=[]
        result=compare.summarize([a,b])
        self.assertEqual(result["pairs"][1]["intersection_pages"],1)
        self.assertEqual(result["pairs"][1]["union_pages"],2)
        self.assertEqual(result["observed_only_pairs"][1]["intersection_pages"],0)
    def test_publication_failure_releases_policy_hold_and_invalidates_measurement(self):
        with tempfile.TemporaryDirectory() as d:
            o,calls,_=self.make(d);o.session_start();o.arm("one")
            o._publish=lambda bundle: (_ for _ in ()).throw(OSError("disk failed"))
            with self.assertRaises(OSError):o.collect()
            self.assertFalse(o.paused);self.assertTrue(o.invalid)
            self.assertEqual(calls[-1],"Resumed");o.close()
    def test_bitmap_bounds_union_and_epoch(self):
        regions=SLOTS+[{"guest_phys_addr":32768,"memory_size":4096,"userspace_addr":131072}]
        self.assertEqual(pack_pages([12288,32768],regions),[(8).to_bytes(8,"little"),(1).to_bytes(8,"little")])
        for g in (1,16384,-4096):
            with self.assertRaises(ValueError):pack_pages([g],regions)
        with self.assertRaisesRegex(ValueError,"epoch"):validate_dirty(response(3,4),2,3,SLOTS)
        bad=response(2,3,16)
        with self.assertRaisesRegex(ValueError,"padding"):validate_dirty(bad,2,3,SLOTS)

if __name__=="__main__":unittest.main()
