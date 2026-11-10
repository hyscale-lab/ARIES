"""Actual HTTP barrier tests with only kernel capture replaced by deterministic fixtures."""
import json
import os
from pathlib import Path
import tempfile
import threading
import time
import unittest
from unittest.mock import patch
import urllib.error
import urllib.request

import serve


class FixtureVmmObserver:
    instances = []

    def __init__(self, pid, vm_id, start, socket_path, config, output):
        self.pid, self.vm_id, self.start = pid, vm_id, start
        self.arm_count = self.collect_count = 0
        self.armed = None
        self.closed = False
        self.instances.append(self)

    def session_start(self):
        return {"ok": True, "paused": True}

    def request_abort(self):
        pass

    def abort_session(self, reason):
        return {"ok": True, "released": True, "paused": False}

    def arm(self, name):
        if self.armed is not None:
            raise AssertionError("double arm")
        begin = time.monotonic_ns()
        self.arm_count += 1
        self.armed = {"name": name, "begin": begin, "end": time.monotonic_ns()}
        return {"arm_begin_ns": begin, "arm_end_ns": self.armed["end"],
                "bitmap_write_end_ns": self.armed["end"], "arm_cpu_ns": 0}

    def collect(self):
        if self.armed is None:
            raise AssertionError("not armed")
        begin = time.monotonic_ns()
        self.collect_count += 1
        arm = self.armed
        self.armed = None
        return {
            "schema": "aries.vmm-tracer.compact.v1", "vm_instance": self.vm_id,
            "vmm_pid": self.pid, "vmm_start_ticks": self.start, "clock": "host-CLOCK_MONOTONIC",
            "page_bytes": 4096, "accessed_gpas": [0], "unknown_gpas": {"4096": "fixture"},
            "idle_gpas": [8192], "arm_begin_ns": arm["begin"], "arm_end_ns": arm["end"],
            "scan_begin_ns": begin, "scan_end_ns": time.monotonic_ns(),
            "bitmap_read_begin_ns": begin, "bitmap_read_end_ns": time.monotonic_ns(),
            "arm_cpu_ns": 0, "scan_cpu_ns": 0,
            "not_resident_at_arm_gpa_ranges": [], "not_resident_at_end_gpa_ranges": [],
        }

    def close(self):
        self.closed = True


class HTTPBarriers(unittest.TestCase):
    def fixture(self, mode, action):
        validation = Path(__file__).resolve().parents[2] / ".cache/tsb-http-tests"
        validation.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=validation) as directory:
            output = Path(directory) / "observer"
            servers, failures = [], []
            started = threading.Event()
            real_http_server = serve.ThreadingHTTPServer
            pid = os.getpid()
            config = Path(directory) / "config.json"
            config.write_text('{"backend":"vmm_owned"}')

            class CaptureServer(real_http_server):
                def __init__(self, *args, **kwargs):
                    super().__init__(*args, **kwargs)
                    servers.append(self)
                    started.set()

            def run():
                try:
                    serve.main()
                except BaseException as error:
                    failures.append(error)

            arguments = ["serve.py", "--output", str(output), "--port", "0",
                         "--vm-prefix", "aries-sandbox-http-", "--mode", mode,
                         "--access-dirty-config", str(config)]
            with patch("sys.argv", arguments), \
                    patch("vmm_observer.discover", return_value=(pid, "aries-sandbox-http-fixture", 1, "fixture.sock")), \
                    patch("vmm_observer.VmmObserver", FixtureVmmObserver), \
                    patch.object(serve, "ThreadingHTTPServer", CaptureServer), \
                    patch.object(serve.signal, "signal"):
                thread = threading.Thread(target=run, daemon=True)
                thread.start()
                self.assertTrue(started.wait(5), failures)
                address = "http://127.0.0.1:" + str(servers[0].server_port)

                def post(path, call=None, batch="batch-1"):
                    payload = {"session_id": "fixture-session", "turn_id": "1",
                               "batch_id": batch, "llm_step_id": batch,
                               "native_session_key": "native", "tool_call_id": call, "tool_name": "exec"}
                    if path == "/batch":
                        payload.update(expected_tool_call_ids=["a", "b"], requested_tools=[
                            {"tool_call_id": c, "tool_name": "exec"} for c in ("a", "b")])
                    request = urllib.request.Request(address + path, data=json.dumps(payload).encode(),
                                                     headers={"Content-Type": "application/json"}, method="POST")
                    with urllib.request.urlopen(request, timeout=5) as response:
                        return json.load(response)

                try:
                    self.assertTrue(post("/session/start")["paused"])
                    action(post, output)
                finally:
                    servers[0].shutdown()
                    thread.join(timeout=5)
                self.assertFalse(thread.is_alive())
                self.assertFalse(failures, failures)
                observer = FixtureVmmObserver.instances[-1]
                self.assertTrue(observer.closed)
                return observer.arm_count, observer.collect_count

    def test_sequential_batch_returns_pending_despite_zero_active_calls(self):
        def action(post, output):
            arm = post("/batch")
            self.assertEqual(arm["collection"], "armed")
            post("/begin", "a")
            first = post("/end", "a")
            self.assertEqual(first["collection"], "pending")
            self.assertEqual(first["remaining_active_tool_call_ids"], [])
            self.assertEqual(first["remaining_expected_tool_call_ids"], ["b"])
            groups = [json.loads(line) for line in (output / "groups.jsonl").read_text().splitlines()]
            self.assertEqual([r["event"] for r in groups], ["batch_begin"])
            post("/begin", "b")
            last = post("/end", "b")
            self.assertEqual(last["collection"], "collected")
            groups = [json.loads(line) for line in (output / "groups.jsonl").read_text().splitlines()]
            self.assertEqual([r["event"] for r in groups], ["batch_begin", "group_end"])
            self.assertEqual(groups[-1]["reading"]["schema"], "aries.vmm-tracer.compact.v1")
            self.assertEqual(groups[-1]["concurrency_peak"], 1)
            steps = [json.loads(line) for line in (output / "steps.jsonl").read_text().splitlines()]
            self.assertTrue(all("reading" not in row for row in steps))
            self.assertEqual((output / "errors.jsonl").read_text(), "")
        self.assertEqual(self.fixture("group", action), (1, 1))

    def test_parallel_batch_accepts_both_calls_without_native_serialization(self):
        def action(post, output):
            post("/batch")
            post("/begin", "a")
            post("/begin", "b")
            post("/end", "a")
            post("/end", "b")
            groups = [json.loads(line) for line in (output / "groups.jsonl").read_text().splitlines()]
            self.assertEqual(groups[-1]["concurrency_peak"], 2)
            self.assertEqual(groups[-1]["member_tool_call_ids"], ["a", "b"])
            self.assertEqual(groups[-1]["reading"]["transition_gaps"], [])
            self.assertEqual((output / "errors.jsonl").read_text(), "")
        self.assertEqual(self.fixture("group", action), (1, 1))

    def test_immediate_results_complete_expected_batch_without_any_begin(self):
        def action(post, output):
            post("/batch")
            post("/end", "a")
            post("/end", "b")
            groups = [json.loads(line) for line in (output / "groups.jsonl").read_text().splitlines()]
            self.assertTrue(all(not row["begin_observed"] for row in groups[-1]["group_members"]))
            steps = [json.loads(line) for line in (output / "steps.jsonl").read_text().splitlines()]
            self.assertEqual([row["event"] for row in steps], ["step_end", "step_end"])
            self.assertEqual((output / "errors.jsonl").read_text(), "")
        self.assertEqual(self.fixture("group", action), (1, 1))


if __name__ == "__main__":
    unittest.main()
