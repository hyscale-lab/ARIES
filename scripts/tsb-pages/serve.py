#!/usr/bin/env python3
"""One shared VM-page footprint for each complete native LLM tool-request batch."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
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
                        help="optional paused host-side attribution configuration")
    parser.add_argument("--access-dirty-config", type=Path, help="opt-in continuous access/dirty epochs with LLM pause policy")
    args = parser.parse_args()
    access_dirty_config = json.loads(args.access_dirty_config.read_text()) if args.access_dirty_config else None
    vmm_owned = access_dirty_config is not None and access_dirty_config.get("backend") == "vmm_owned"
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

    tracer = None
    thread = None
    slots = []
    if not vmm_owned:
        from idle import IdleObserver
        from run import parse_slot
        daemon_pid = int(subprocess.check_output([
            "systemctl", "show", "firecracker-containerd", "--property", "MainPID", "--value"], text=True))
        slots = []
        tracer = subprocess.Popen(["strace", "-f", "-e", "trace=ioctl", "-p", str(daemon_pid)],
                                  stderr=subprocess.PIPE, stdout=subprocess.DEVNULL, text=True)

        def trace():
            for line in tracer.stderr:
                entry = parse_slot(line)
                if entry:
                    slots.append(entry)

        thread = threading.Thread(target=trace)
        thread.start()
        time.sleep(1)
        if tracer.poll() is not None:
            raise RuntimeError("capture attach failed")
    observer = None
    tracker = None
    invalid = False
    operation_lock = threading.RLock()
    identity_lock = threading.Lock()
    error_lock = threading.Lock()
    abort_requested = threading.Event()
    session_identity = None

    def setup():
        nonlocal observer, tracer, tracker
        if vmm_owned:
            from vmm_observer import VmmObserver, discover
            vmm, vm_id, start, socket_path = discover(args.vm_prefix)
            config = dict(access_dirty_config)
            if attribution_config is not None:
                config["attribution"] = attribution_config
            observer = VmmObserver(vmm, vm_id, start, socket_path, config, args.output / "access-dirty")
        else:
            tracer.send_signal(signal.SIGINT)
            tracer.wait(timeout=10)
            thread.join(timeout=10)
            tracer = None
            (args.output / "slots.json").write_text(json.dumps(slots, indent=2) + "\n")
            candidates = {}
            for entry in slots:
                try:
                    pid = int(next(line.split()[1]
                                   for line in Path("/proc/" + str(entry["tid"]) + "/status").read_text().splitlines()
                                   if line.startswith("Tgid:")))
                    command = Path("/proc/" + str(pid) + "/cmdline").read_bytes().split(b"\0")
                    at = command.index(b"--id")
                    vm_id = command[at + 1].decode()
                except (OSError, ValueError, IndexError):
                    continue
                if vm_id.startswith(args.vm_prefix):
                    candidates.setdefault((pid, vm_id), []).append(entry)
            if len(candidates) != 1:
                raise RuntimeError("require exactly one captured live VMM matching study sandbox prefix")
            (vmm, vm_id), regions = next(iter(candidates.items()))
            (args.output / "selected-slots.json").write_text(json.dumps(regions, indent=2) + "\n")
            observer = IdleObserver(vmm, vm_id, regions)
            if access_dirty_config is not None:
                import sys
                sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tsb-attribution"))
                from access_dirty import AccessDirtyObserver
                config = dict(access_dirty_config)
                if attribution_config is not None:
                    config["attribution"] = attribution_config
                observer = AccessDirtyObserver(observer, config, args.output / "access-dirty")
            elif attribution_config is not None:
                import sys
                sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tsb-attribution"))
                from paused import PausedAttributionObserver
                observer = PausedAttributionObserver(observer, attribution_config, args.output / "attribution")
        tracker = BatchTracker(observer, lambda value: save(groups, value))
        tracker.invalid = invalid
        (args.output / "vmm.json").write_text(json.dumps({
            "pid": vmm, "vm_id": vm_id, "start_ticks": observer.start,
            "scope": "single VM identity; persistent mode only", "backend": "vmm_owned" if vmm_owned else "legacy_external"}) + "\n")

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
                        if current is not None and hasattr(current, "request_abort"):
                            current.request_abort()
                with operation_lock:
                    if session_identity is not None and identity != session_identity:
                        raise ValueError("observer session identity mismatch")
                    if abort_requested.is_set() and self.path != "/session/abort":
                        raise ValueError("host cancellation already requested")
                    if self.path == "/session/start":
                        if access_dirty_config is None:
                            raise ValueError("session pause requires access-dirty mode")
                        if session_identity is None: session_identity = identity
                        if observer is None: setup()
                        if abort_requested.is_set():
                            observer.request_abort()
                            raise ValueError("host cancellation interrupted initialization")
                        tracker.last_ack = observer.session_start()
                    elif self.path == "/session/abort":
                        if access_dirty_config is None:
                            raise ValueError("host abort requires access-dirty mode")
                        if observer is None:
                            # No observer was initialized and therefore no pause
                            # or capture ownership was acquired by this endpoint.
                            result = {"ok": True, "released": True, "paused": False,
                                      "measurement_valid": False, "terminal_complete": False,
                                      "observer_not_initialized": True}
                        else:
                            if not hasattr(observer, "abort_session"):
                                raise ValueError("backend has no bounded cancellation barrier")
                            result = observer.abort_session(body.get("reason", "host_confirmed_harness_stopped"))
                        result = {**result, "session_id": identity[0], "turn_id": identity[1]}
                        if tracker: tracker.last_ack = result
                    elif self.path == "/session/end":
                        if access_dirty_config is None or observer is None:
                            raise ValueError("no initialized access-dirty session")
                        tracker.last_ack = observer.session_end(body.get("reason", "native_terminal"), body.get("terminal_complete") is True)
                    elif self.path == "/batch":
                        if observer is None: setup()
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
                if vmm_owned:
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
                        if vmm_owned and abort_requested.is_set():
                            observer.abort_session("observer_shutdown")
                    finally:
                        observer.close()
                if tracer and tracer.poll() is None:
                    tracer.send_signal(signal.SIGINT)
                    tracer.wait(timeout=10)
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
