#!/usr/bin/env python3
"""One shared VM-page footprint for each complete native LLM tool-request batch."""
import argparse
import json
import os
from pathlib import Path
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from batches import SCHEMA, SCOPE, VALIDITY_SCOPE, BatchTracker


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--port", type=int, default=18181)
    parser.add_argument("--vm-prefix", default="aries-sandbox-fix-git-",
                        help="exact expected study sandbox ID prefix")
    parser.add_argument("--mode", choices=("group",), default="group",
                        help="one shared reading per native assistant tool-request batch")
    parser.add_argument("--attribution-config", type=Path,
                        help="optional VMM companion attribution configuration")
    parser.add_argument("--access-dirty-config", type=Path, required=True,
                        help="VMM-owned access/dirty configuration with backend=vmm_owned")
    args = parser.parse_args()
    access_dirty_config = json.loads(args.access_dirty_config.read_text())
    if not isinstance(access_dirty_config, dict) or access_dirty_config.get("backend") != "vmm_owned":
        raise ValueError("observer requires backend=vmm_owned")
    attribution_config = json.loads(args.attribution_config.read_text()) if args.attribution_config else None
    mode_schema, mode_scope, validity_scope = SCHEMA, SCOPE, VALIDITY_SCOPE
    os.umask(0o077)
    args.output.mkdir(parents=True, exist_ok=False)
    rows = (args.output / "steps.jsonl").open("x")
    errors = (args.output / "errors.jsonl").open("x")
    groups = (args.output / "groups.jsonl").open("x")

    def save(stream, value):
        stream.write(json.dumps(value) + "\n")
        stream.flush()

    observer = None
    tracker = None
    invalid = False
    operation_lock = threading.RLock()
    identity_lock = threading.Lock()
    error_lock = threading.Lock()
    abort_requested = threading.Event()
    session_identity = None

    def setup():
        nonlocal observer, tracker
        from vmm_observer import VmmObserver, discover
        vmm, vm_id, start, socket_path = discover(args.vm_prefix)
        config = dict(access_dirty_config)
        if attribution_config is not None:
            config["attribution"] = attribution_config
        observer = VmmObserver(vmm, vm_id, start, socket_path, config, args.output / "access-dirty")
        tracker = BatchTracker(observer, lambda value: save(groups, value))
        tracker.invalid = invalid
        (args.output / "vmm.json").write_text(json.dumps({
            "pid": vmm, "vm_id": vm_id, "start_ticks": observer.start,
            "scope": "single VM identity; persistent mode only", "backend": "vmm_owned"}) + "\n")

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def setup(self):
            super().setup()
            self.connection.settimeout(30)

        def do_GET(self):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(json.dumps({
                "ready": True, "active": bool(tracker and tracker.active),
                "active_count": len(tracker.active) if tracker else 0,
                "pending_batch": bool(tracker and tracker.pending is not None),
                "invalid": invalid, "reading_schema": mode_schema, "mode": args.mode,
                "coverage_complete": False, "measurement_valid_scope": validity_scope,
            }).encode())

        def do_POST(self):
            nonlocal invalid, session_identity
            body = {}
            status = 200
            ended_ns = time.monotonic_ns()
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size < 16384:
                    raise ValueError("invalid metadata length")
                body = json.loads(self.rfile.read(size))
                identity = tuple(body.get(name) for name in ("session_id", "turn_id"))
                if not all(isinstance(value, str) and value for value in identity):
                    raise ValueError("native session/turn hierarchy required")
                with identity_lock:
                    if session_identity is not None and identity != session_identity:
                        raise ValueError("observer session identity mismatch")
                    if session_identity is None and self.path in ("/session/start", "/session/abort", "/batch"):
                        session_identity = identity
                    if self.path == "/session/abort":
                        # Bind/validate identity before cancellation can affect
                        # a concurrently initializing session. Never wait for the
                        # operation lock before interrupting its live reads.
                        abort_requested.set()
                        current = observer
                        if current is not None:
                            current.request_abort()
                with operation_lock:
                    if session_identity is not None and identity != session_identity:
                        raise ValueError("observer session identity mismatch")
                    if abort_requested.is_set() and self.path != "/session/abort":
                        raise ValueError("host cancellation already requested")
                    if self.path == "/session/start":
                        if session_identity is None: session_identity = identity
                        if observer is None: setup()
                        if abort_requested.is_set():
                            observer.request_abort()
                            raise ValueError("host cancellation interrupted initialization")
                        tracker.last_ack = observer.session_start()
                    elif self.path == "/session/abort":
                        if observer is None:
                            # No observer was initialized and therefore no pause
                            # or capture ownership was acquired by this endpoint.
                            result = {"ok": True, "released": True, "paused": False,
                                      "measurement_valid": False, "terminal_complete": False,
                                      "observer_not_initialized": True}
                        else:
                            result = observer.abort_session(body.get("reason", "host_confirmed_harness_stopped"))
                        result = {**result, "session_id": identity[0], "turn_id": identity[1]}
                        if tracker: tracker.last_ack = result
                    elif self.path == "/session/end":
                        if observer is None:
                            raise ValueError("no initialized access-dirty session")
                        tracker.last_ack = observer.session_end(body.get("reason", "native_terminal"), body.get("terminal_complete") is True)
                    elif self.path == "/batch":
                        if observer is None:
                            raise ValueError("no initialized VMM-owned session")
                        tracker.register(body)
                    elif self.path in ("/begin", "/end"):
                        key = tuple(body.get(name) for name in ("session_id", "turn_id", "tool_call_id"))
                        if not all(isinstance(value, str) and value for value in key):
                            raise ValueError("native call hierarchy IDs required")
                        if tracker is None:
                            raise ValueError("native call/result has no registered LLM batch")
                        row = tracker.begin(key, body) if self.path == "/begin" else tracker.end(key, body, ended_ns)
                        save(rows, row)
                    else:
                        raise ValueError("unknown endpoint")
                    if self.path != "/session/abort": result = tracker.last_ack
                    result = {**result, "session_id": session_identity[0], "turn_id": session_identity[1]}
            except Exception as error:
                status = 409
                invalid = True
                if tracker: tracker.invalid = True
                result = {"ok": False, "error": str(error)}
                with error_lock:
                    save(errors, {"host_monotonic_ns": time.monotonic_ns(), "path": self.path,
                                  "metadata": body, "error": str(error)})
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(result).encode())

    # Register the complete assistant-requested ID list before native execution.
    # Individual admissions/results never reset the shared batch bitmap.
    # Native sequential/parallel scheduling and expanded operations are untouched.
    class ObserverServer(ThreadingHTTPServer):
        daemon_threads = False
    server = ObserverServer(("0.0.0.0", args.port), Handler)
    (args.output / "ready.json").write_text(json.dumps({
        "port": args.port, "pid": os.getpid(), "clock": "host monotonic",
        "reading_schema": mode_schema, "measurement": mode_scope, "mode": args.mode,
        "coverage_complete": False, "measurement_valid_scope": validity_scope,
        "hierarchy": "session=e2e task; turn=user-harness; LLM step=one tool-request batch; native calls and bridge operations are members",
        "concurrency": "native sequential/parallel scheduling is preserved; expected result IDs define batch completion",
    }) + "\n")

    def stop(*_):
        def shutdown():
            try:
                abort_requested.set()
                current = observer
                if current is not None: current.request_abort()
            finally:
                server.shutdown()
        # Signals may interrupt cleanup while it owns an observer lock. Keep
        # interruption before handler joining, outside the signal context.
        threading.Thread(target=shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        server.serve_forever(poll_interval=.2)
    finally:
        # Stop admissions and join bounded request handlers before closing logs.
        server.server_close()
        with operation_lock:
            try:
                if tracker and tracker.pending is not None:
                    pending = tracker.pending
                    save(errors, {"error": "shutdown with incomplete LLM batch",
                                  "metadata": pending["metadata"],
                                  "completed_tool_call_ids": pending["completed_order"],
                                  "missing_tool_call_ids": [call for call in pending["expected"]
                                                            if call not in pending["completed"]]})
                if observer:
                    try:
                        if abort_requested.is_set():
                            observer.abort_session("observer_shutdown")
                    finally:
                        observer.close()
            except BaseException as error:
                save(errors, {"error": "observer shutdown failed",
                              "type": type(error).__name__, "message": str(error)})
                raise
            finally:
                rows.close()
                errors.close()
                groups.close()


if __name__ == "__main__":
    main()
