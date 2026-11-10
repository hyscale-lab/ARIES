"""Pause-only attribution adapter for the native LLM-batch observer."""
import http.client
import importlib.util
import json
import socket
import time
from pathlib import Path


class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.path = str(path)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class PausedAttributionObserver:
    def __init__(self, observer, config, output, decoder_factory=None):
        self.base = observer
        self.pid, self.vm_id = observer.pid, observer.vm_id
        self.slots, self.start = observer.slots, observer.start
        self.config = dict(config)
        self.output = Path(output)
        self.output.mkdir(parents=True, exist_ok=False)
        self.paused = False
        self.bootstrapped = False
        self.serial = 0
        if decoder_factory is None:
            path = Path(__file__).with_name("decoder.py")
            spec = importlib.util.spec_from_file_location("aries_guest_decoder", path)
            module = importlib.util.module_from_spec(spec)
            # Some decoder dependencies resolve relative to their own directory.
            import sys
            sys.path.insert(0, str(path.parent))
            sys.modules[spec.name] = module
            spec.loader.exec_module(module)
            decoder_factory = module.GuestDecoder
        self.decoder = decoder_factory(self.pid, self.vm_id, self.slots, self.config)
        try:
            self.socket_path = self.resolve_socket()
            self.events = (self.output / "boundaries.jsonl").open("x")
        except BaseException:
            close = getattr(self.decoder, "close", None)
            if close:
                close()
            raise

    def resolve_socket(self):
        supplied = self.config.get("socket_path")
        template = self.config.get("socket_template")
        if supplied or template:
            return str(supplied or template.format(vm_id=self.vm_id, pid=self.pid))
        argv = Path(f"/proc/{self.pid}/cmdline").read_bytes().split(b"\0")
        try:
            at = argv.index(b"--api-sock")
            value = Path(argv[at + 1].decode())
        except (ValueError, IndexError):
            raise RuntimeError("Firecracker --api-sock not found; provide socket_template")
        if value.is_absolute():
            return str(Path(f"/proc/{self.pid}/root") / value.relative_to("/"))
        return str(Path(f"/proc/{self.pid}/cwd") / value)

    def _save(self, name, value):
        (self.output / name).write_text(json.dumps(value, indent=2) + "\n")

    def _state(self, state):
        self.base.identity()
        conn = UnixHTTPConnection(self.socket_path, self.config.get("api_timeout_seconds", 10))
        try:
            conn.request("PATCH", "/vm", json.dumps({"state": state}),
                         {"Content-Type": "application/json"})
            response = conn.getresponse()
            body = response.read()
            if response.status != 204:
                raise RuntimeError(f"Firecracker {state}: HTTP {response.status}: {body[:512]!r}")
        finally:
            conn.close()
        self.base.identity()

    def _boundary(self, kind, action):
        event = {"schema": "aries.tsb.attribution-boundary.v1", "kind": kind,
                 "vm_id": self.vm_id, "vmm_pid": self.pid, "vmm_start_ticks": self.start,
                 "begin_ns": time.monotonic_ns()}
        # A timed-out pause may have taken effect; always attempt resume after it.
        self.paused = True
        try:
            self._state("Paused")
            event["pause_ack_ns"] = time.monotonic_ns()
            result = action(event)
            event["capture_end_ns"] = time.monotonic_ns()
            return result, event
        except BaseException as error:
            event["error"] = {"type": type(error).__name__, "message": str(error)}
            raise
        finally:
            try:
                self._state("Resumed")
                self.paused = False
                event["resume_ack_ns"] = time.monotonic_ns()
            except BaseException as error:
                event["resume_error"] = {"type": type(error).__name__, "message": str(error)}
                raise
            finally:
                event["end_ns"] = time.monotonic_ns()
                self.events.write(json.dumps(event) + "\n")
                self.events.flush()

    def arm(self, tool_call_id=None):
        def action(event):
            if not self.bootstrapped:
                result = self.decoder.bootstrap()
                self._save("bootstrap.json", result)
                self.bootstrapped = True
                event["bootstrap_completed_ns"] = time.monotonic_ns()
            result=self.base.arm(tool_call_id)
            event['resident_pages_at_arm']=result.get('resident_pages_at_arm')
            return result
        result, boundary = self._boundary("arm", action)
        result = dict(result)
        result["boundary_pause"] = boundary
        return result

    def collect(self):
        self.serial += 1
        directory = self.output / f"step-{self.serial:06d}"
        directory.mkdir()
        def action(event):
            reading = self.base.collect()
            # Preserve the finalized access evidence even if attribution fails.
            (directory / "reading.json").write_text(json.dumps(reading) + "\n")
            event["bitmap_collected_ns"] = time.monotonic_ns()
            attribution = self.decoder.capture(reading, directory)
            (directory / "capture.json").write_text(json.dumps(attribution, indent=2) + "\n")
            reading["attribution"] = attribution
            reading["attribution_directory"] = str(directory)
            return reading
        result, boundary = self._boundary("collect", action)
        result["boundary_pause"] = boundary
        return result

    def close(self):
        try:
            if self.paused:
                self._state("Resumed")
                self.paused = False
        finally:
            try:
                close = getattr(self.decoder, "close", None)
                if close:
                    close()
            finally:
                self.events.close()
                self.base.close()
