import threading
from http.server import BaseHTTPRequestHandler
import socketserver
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, call

from idle_hook import Firecracker, frozen_capture, select_slots, take_boundary, MIB


class SlotTests(unittest.TestCase):
    def test_restored_identity_excludes_starter_failed_ioctls_and_device_slots(self):
        capture = """ready 8000
slot 9100 9200 141 9 0 0 0 2097152 8192 0
slot 11000 12000 140 9 0 0 0 2097152 8192 0
slot 11010 12010 141 10 0 1 0 1048576 8192 0
slot 11020 12020 141 10 1 0 1048576 1048576 1060864 0
slot 11030 12030 141 10 2 2 274877906944 1048576 2113536 0
slot 11040 12040 141 10 1 0 1048576 0 0 -22
"""
        dram, other = select_slots(capture, 141, "10", 1000, 2 * MIB)
        self.assertEqual([(s["guest_phys_addr"], s["userspace_addr"]) for s in dram],
                         [(0, 8192), (MIB, 1060864)])
        self.assertEqual([s["slot"] for s in other], [2])

    def test_capture_must_be_armed_before_process_birth(self):
        capture = "ready 10001\nslot 11000 12000 141 10 0 0 0 1048576 8192 0"
        with self.assertRaisesRegex(RuntimeError, "not ready"):
            select_slots(capture, 141, "10", 1000, MIB)

    def test_missing_registration_cannot_produce_partial_success(self):
        capture = "ready 9999\nslot 11000 12000 141 10 0 0 0 1048576 8192 0"
        with self.assertRaisesRegex(RuntimeError, "incomplete"):
            select_slots(capture, 141, "10", 1000, 2 * MIB)
        with self.assertRaisesRegex(RuntimeError, "ended"):
            select_slots(capture + "\nend 12001", 141, "10", 1000, MIB)


class FreezeTests(unittest.TestCase):
    def setUp(self):
        self.api = Mock()
        self.observer = Mock()

    def test_capture_then_rearm_happen_while_vm_paused(self):
        calls = Mock()
        calls.attach_mock(self.api, "api")
        calls.attach_mock(self.observer, "observer")
        event = {}
        frozen_capture(self.api, self.observer, "pre-tool", 18, event)
        self.assertEqual(calls.mock_calls, [
            call.api.request("PATCH", "/vm", "Paused"),
            call.observer.collect(),
            call.observer.arm(18),
            call.api.request("PATCH", "/vm", "Resumed"),
        ])
        self.assertEqual(list(event["freeze"]), [
            "pause_request_ns", "pause_ack_ns", "resume_request_ns", "resume_ack_ns",
        ])
        self.assertEqual(list(event["freeze"].values()), sorted(event["freeze"].values()))

    def test_failed_capture_releases_vm_without_rearming(self):
        self.observer.collect.side_effect = OSError("scan failed")
        with self.assertRaisesRegex(OSError, "scan failed"):
            frozen_capture(self.api, self.observer, "post-tool", 18, {})
        self.api.request.assert_has_calls([
            call("PATCH", "/vm", "Paused"), call("PATCH", "/vm", "Resumed"),
        ])
        self.observer.arm.assert_not_called()

    def test_failed_pause_ack_still_attempts_resume(self):
        self.api.request.side_effect = [OSError("lost pause ack"), None]
        with self.assertRaisesRegex(OSError, "lost pause ack"):
            frozen_capture(self.api, self.observer, "session-start", 0, {})
        self.api.request.assert_has_calls([
            call("PATCH", "/vm", "Paused"), call("PATCH", "/vm", "Resumed"),
        ])
        self.observer.arm.assert_not_called()

    def test_boundary_classifies_wait_and_tool_using_matched_step(self):
        wait = {}
        take_boundary("pre-tool", 2, self.api, self.observer,
                      {"phase": "session-start", "step": 0, "armed": {"before": {}}}, wait)
        self.assertEqual(wait["window_kind"], "wait")
        tool = {}
        take_boundary("post-tool", 2, self.api, self.observer,
                      {"phase": "pre-tool", "step": 2, "armed": {"before": {}}}, tool)
        self.assertEqual(tool["window_kind"], "tool")
        with self.assertRaisesRegex(RuntimeError, "unexpected trace boundary"):
            take_boundary("post-tool", 3, self.api, self.observer,
                          {"phase": "pre-tool", "step": 2, "armed": {"before": {}}}, {})

    def test_intra_tool_rearms_same_step_and_classifies_tail(self):
        for prior in ("pre-tool", "intra-tool"):
            window = {}
            take_boundary("intra-tool", 12, self.api, self.observer,
                          {"phase": prior, "step": 12, "armed": {"before": {}}}, window)
            self.assertEqual(window["window_kind"], "intra-tool")
            self.observer.arm.assert_called_with(12)
        tail = {}
        take_boundary("post-tool", 12, self.api, self.observer,
                      {"phase": "intra-tool", "step": 12, "armed": {"before": {}}}, tail)
        self.assertEqual(tail["window_kind"], "tool-tail")
        self.observer.arm.assert_called_with(None)
        with self.assertRaisesRegex(RuntimeError, "unexpected trace boundary"):
            take_boundary("intra-tool", 13, self.api, self.observer,
                          {"phase": "intra-tool", "step": 12, "armed": {"before": {}}}, {})


class ApiTests(unittest.TestCase):
    def test_unix_http_binds_same_peer_on_long_paths(self):
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"state":"Running","id":"firecracker","vmm_version":"1.18.0"}')

        with tempfile.TemporaryDirectory() as temp:
            sub = Path(temp) / ("a" * 96)
            sub.mkdir()
            # Create the test socket via a short symlink, outside any real VMM.
            short = Path(temp) / "short"
            short.symlink_to(sub, target_is_directory=True)
            with socketserver.UnixStreamServer(str(short / "api"), Handler) as server:
                serve = threading.Thread(target=server.serve_forever)
                serve.start()
                try:
                    api = Firecracker(str(sub / "api"))
                    self.assertEqual(api.inspect()["state"], "Running")
                    self.assertEqual(api.request("GET", "/")["id"], "firecracker")
                    self.assertIsNotNone(api.identity)
                finally:
                    server.shutdown()
                    serve.join()


if __name__ == "__main__":
    unittest.main()
