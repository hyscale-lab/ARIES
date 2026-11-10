import ast
import io
import gzip
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
from types import SimpleNamespace
from vmm_observer import VmmObserver
class Tests(unittest.TestCase):
    def observer(self, root):
        o=object.__new__(VmmObserver)
        o.output=Path(root);o.config={};o.pending=None;o.publications=[]
        o.coordinator=SimpleNamespace(epoch=4,generation="g")
        o.invalid=False;o.session_ended=False;o.paused=True;o.phase="llm_wait"
        o.events=io.StringIO();o.api=SimpleNamespace(calls=[],call=Mock(return_value={"schema":"aries.firecracker.metadata-capture.v1","vm_generation":"g","capture_id":"c","epoch":4,"state":"sealed","consistency":"pending"}))
        o.vm_id="v";o.pid=1;o.start=2
        o._state=Mock(side_effect=lambda state,event:setattr(o,"paused",state=="Paused"))
        o._finish_publications=Mock()
        return o
    def test_no_resume_with_live_reader_if_abort_fails(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o.pending={"child":Mock(capture_id="c",epoch=4)}
            o._rotate=Mock(side_effect=RuntimeError("rotate failed"))
            o.pending["child"].discard_for_cleanup.side_effect=RuntimeError("cannot reap")
            with self.assertRaises(RuntimeError):o.session_end()
            o._state.assert_not_called()
            self.assertTrue(o.invalid)
    def test_failed_rotation_discard_before_cleanup_resume(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);events=[]
            child=Mock(capture_id="c",epoch=4);child.discard_for_cleanup.side_effect=lambda reason:events.append("discard") or {"discarded":True,"reader_process_reaped":True}
            o.pending={"child":child};o._rotate=Mock(side_effect=RuntimeError("poisoned"))
            o._state.side_effect=lambda state,event:events.append(state)
            with self.assertRaises(RuntimeError):o.session_end()
            self.assertEqual(events,["discard","Resumed"])
    def test_final_clean_gap_complete_and_resumed(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o._rotate=Mock()
            result=o.session_end(terminal_complete=True)
            self.assertTrue(result["terminal_complete"]);self.assertFalse(result["paused"])
            self.assertFalse(o._rotate.call_args.kwargs["censored"])
    def test_tool_tail_is_censored(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o.phase="tool";o._rotate=Mock()
            o.session_end(terminal_complete=True)
            self.assertTrue(o._rotate.call_args.kwargs["censored"])
    def test_close_reaps_sealed_publication_after_session_failure(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o.closed=False;o.phases=io.StringIO()
            child=Mock();child.sealed=True;child.process.poll.return_value=None
            o.publications=[{"child":child}]
            failure=RuntimeError("original terminal error")
            o.session_end=Mock(side_effect=failure)
            with self.assertRaisesRegex(RuntimeError,"original terminal error"):o.close()
            child.abort.assert_called_once()
            self.assertTrue(o.closed)
    def test_finalized_publication_failure_retains_server_record(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);child=Mock(capture_id="c",epoch=4)
            o.pending={"child":child}
            child.abort.return_value={"reader_process_reaped":True}
            o.api.call.return_value["consistency"]="valid"
            o._cleanup_capture()
            child.abort.assert_called_once()
            child.discard_for_cleanup.assert_not_called()
            self.assertIsNone(o.pending)
    def test_compact_absence_unknown_and_dirty_only_roundtrip(self):
        from compact import expand_reading
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o.bootstrap=None;o.current_group="step"
            o.coordinator.regions=[{"slot":0,"guest_base":0,"bytes":16384,"page_count":4}]
            access=[{"gpa":0,"state":"observed"},{"gpa":4096,"state":"unknown","unknown_reason":"newly-resident-unarmed"},
                    {"gpa":8192,"state":"idle"},{"gpa":12288,"state":"absent"}]
            dirty=[{"gpa":8192,"kvm_dirty":True,"vmm_dirty":False}]
            for name,data in (("access",access),("dirty",dirty)):
                with gzip.open(Path(root)/(name+".jsonl.gz"),"wt") as stream:
                    for row in data:stream.write(json.dumps(row)+"\n")
            reading=o._reading({"epoch":3,"epoch_record_id":"record"},Path(root))
            expanded=expand_reading(reading,artifact_directory=root)
            # A relocated archive must not rely on the original worker path.
            import shutil
            from compare import normalize
            destination=Path(root)/"relocated"/"access-dirty"/"epoch-000003"
            destination.mkdir(parents=True)
            for mask in Path(root).glob("*.bin"):shutil.copy2(mask,destination/mask.name)
            reading["artifact_directory"]="/unavailable/disposable/worker"
            normalized=normalize({"event":"group_end","reading":reading,"group_id":"step"},0,
                                 artifact_root=Path(root)/"relocated")
            self.assertEqual(normalized["record_kind"],"llm_reasoning_step")
            self.assertEqual(normalized["counted_touched_pages"],3)
            self.assertEqual(normalized["source_epoch"],3)
            self.assertEqual(expanded["unknown_gpas"],{"4096":"newly-resident-unarmed"})
            self.assertEqual(expanded["additional_gpas"],[8192])
            self.assertEqual(reading["counted_pages"],3)
            self.assertNotIn(12288,expanded["accessed_gpas"])



class CancellationTests(Tests):
    def observer(self,root):
        import threading
        o=super().observer(root)
        o.group="llm-step-000002";o.release_confirmed=False;o.terminal_complete=False
        o.abort_requested=threading.Event();o._connection_lock=threading.Lock()
        o._children_lock=threading.Lock();o._children=[];o._active_connection=None
        o._cleanup_mode=False
        connection=Mock()
        connection.getresponse.return_value.status=200
        connection.getresponse.return_value.read.return_value=b'{"state":"Paused"}'
        o._connection=Mock(return_value=connection)
        o._disconnect=Mock()
        o.identity=Mock()
        return o
    def test_cancellation_abandons_epoch_and_is_idempotent(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o._rotate=Mock()
            result=o.abort_session()
            self.assertTrue(result["released"]);self.assertFalse(result["measurement_valid"])
            self.assertFalse(result["terminal_complete"]);o._rotate.assert_not_called()
            self.assertEqual(o.coordinator.epoch,4)
            self.assertEqual(o.abort_session(),{**result,"already_ended":True})
            o._state.assert_called_once()
    def test_prior_complete_terminal_seal_is_preserved(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o.release_confirmed=o.session_ended=o.terminal_complete=True;o.paused=False
            result=o.abort_session()
            self.assertTrue(result["measurement_valid"]);self.assertTrue(result["terminal_complete"])
            o._state.assert_not_called()
    def test_ambiguous_reader_closure_blocks_resume(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);child=Mock(capture_id="c",epoch=4)
            child.discard_for_cleanup.return_value={"reader_process_reaped":False}
            o.pending={"child":child}
            with self.assertRaisesRegex(RuntimeError,"termination unconfirmed"):o.abort_session()
            o._state.assert_not_called();self.assertFalse(o.release_confirmed)
    def test_lost_resume_ack_retry_never_recollects(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o._rotate=Mock()
            o._state.side_effect=[RuntimeError("lost ACK"),None]
            with self.assertRaisesRegex(RuntimeError,"lost ACK"):o.abort_session()
            self.assertFalse(o.release_confirmed)
            self.assertTrue(o.abort_session()["released"]);o._rotate.assert_not_called()
    def test_replayed_abort_never_interrupts_cleanup(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);connection=Mock();o._active_connection=connection
            o.request_abort();self.assertTrue(o.abort_requested.is_set())
            self.assertEqual(connection.sock.shutdown.call_count,1)
            o._cleanup_mode=True;o.request_abort()
            self.assertEqual(connection.sock.shutdown.call_count,1)
    def test_raw_mask_materialization_honors_cancellation(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o.abort_requested.set()
            with self.assertRaisesRegex(RuntimeError,"cancellation"):
                o._reading({},Path(root))
    def test_capture_creation_after_abort_is_forbidden(self):
        with tempfile.TemporaryDirectory() as root:
            o=self.observer(root);o.Capture=Mock();o.abort_requested.set()
            with self.assertRaisesRegex(RuntimeError,"cancellation"):o._capture()
            o.Capture.assert_not_called()

if __name__=="__main__":unittest.main()
