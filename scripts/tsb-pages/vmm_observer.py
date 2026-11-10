"""Opt-in VMM-owned tracing. No external process-memory, pagemap or idle access."""
import gzip
import hashlib
import http.client
import json
import os
from pathlib import Path
import socket
import sys
import threading
import signal
import time

def save(path, value):
    path = Path(path)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("w") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)

def rows(path):
    with gzip.open(path, "rt") as stream:
        for line in stream:
            if line.strip(): yield json.loads(line)

def process_start(pid):
    return int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])

def discover(prefix):
    matches = []
    for directory in Path("/proc").iterdir():
        if not directory.name.isdigit(): continue
        try:
            argv = (directory / "cmdline").read_bytes().split(b"\0")
            vm_id = argv[argv.index(b"--id") + 1].decode()
            if not vm_id.startswith(prefix): continue
            api = Path(argv[argv.index(b"--api-sock") + 1].decode())
            path = directory / ("root" if api.is_absolute() else "cwd")
            path /= api.relative_to("/") if api.is_absolute() else api
            pid = int(directory.name)
            matches.append((pid, vm_id, process_start(pid), str(path)))
        except (OSError, ValueError, IndexError): continue
    if len(matches) != 1:
        raise RuntimeError("require exactly one live VMM matching the sandbox prefix")
    return matches[0]

class Connection(http.client.HTTPConnection):
    def __init__(self, path, timeout):
        super().__init__("localhost", timeout=timeout)
        self.path = str(path)
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)

class VmmObserver:
    def __init__(self, pid, vm_id, start, socket_path, config, output):
        self.pid, self.vm_id, self.start = pid, vm_id, start
        self.socket_path, self.config = socket_path, dict(config)
        self.output = Path(output)
        self.output.mkdir(parents=True, exist_ok=False)
        self.companion = Path(config.get("companion_path",
            Path(__file__).resolve().parents[1] / "tsb-companion")).resolve()
        sys.path.insert(0, str(self.companion))
        from vmm_api import VmmAPI, ack_record, finalize_capture, export_rows, VmmRequestError
        from epochs import EpochCoordinator
        from process_adapter import ExternalCapture
        self.RequestError = VmmRequestError
        self.abort_requested = threading.Event()
        self._connection_lock = threading.Lock()
        self._children_lock = threading.Lock()
        self._children = []
        self._active_connection = None
        self._cleanup_mode = False
        owner = self
        class CancelableAPI(VmmAPI):
            def call(api, endpoint, operation, **fields):
                owner._check_cancel()
                owner.identity()
                conn = owner._connection()
                request = {"operation": operation, **fields}
                receipt = {"endpoint": endpoint, "operation": operation,
                           "begin_ns": time.monotonic_ns(), "outcome": "failed"}
                try:
                    conn.request("PUT", endpoint, json.dumps(request), {"Content-Type": "application/json"})
                    response = conn.getresponse()
                    receipt["http_status"] = response.status
                    raw = response.read(api.max_response_bytes + 1)
                    receipt["response_bytes"] = len(raw)
                    if len(raw) > api.max_response_bytes: raise RuntimeError("VMM response exceeds client bound")
                    if response.status != 200:
                        try: body = json.loads(raw)
                        except (ValueError, UnicodeError): body = {"unparsed_response": raw.decode(errors="replace")}
                        receipt["error_response"] = body
                        raise VmmRequestError(endpoint, operation, response.status, body)
                    result = json.loads(raw)
                    if not isinstance(result, dict): raise RuntimeError("invalid VMM response type")
                    owner.identity()
                    owner._check_cancel()
                    receipt["outcome"] = "returned"
                    return result
                finally:
                    owner._disconnect(conn)
                    receipt["end_ns"] = time.monotonic_ns()
                    api.calls.append(receipt)
        self.api = CancelableAPI(socket_path, timeout=config.get("api_timeout_seconds", 30))
        self.coordinator = EpochCoordinator(self.api, ["dirty", "idle", "census", "metadata"])
        self.Capture = ExternalCapture
        self.ack_record, self.finalize_capture, self.export_rows = ack_record, finalize_capture, export_rows
        self.paused = self.invalid = self.closed = self.session_ended = False
        self.phase = self.group = self.pending = None
        self.publications = []
        self.bootstrap = None
        self.attribution_config = config.get("attribution")
        if self.attribution_config:
            if self.attribution_config.get("enable_compression"):
                raise ValueError("payload compression is outside this study")
            save(self.output / "attribution-config.json", self.attribution_config)
        self.events = (self.output / "boundaries.jsonl").open("x")
        self.phases = (self.output / "phases.jsonl").open("x")

    def _check_cancel(self):
        if getattr(self, "abort_requested", None) is not None and self.abort_requested.is_set() and not self._cleanup_mode:
            raise RuntimeError("host cancellation requested; current epoch remains censored")

    def _connection(self):
        owner = self
        class GuardedConnection(Connection):
            def connect(connection):
                owner._check_cancel()
                super().connect()
                owner._check_cancel()
        self._check_cancel()
        timeout = self.config.get("api_timeout_seconds", 30)
        if self._cleanup_mode:
            remaining = self._cleanup_deadline - time.monotonic()
            if remaining <= 0: raise TimeoutError("observer cancellation cleanup budget exhausted")
            timeout = min(timeout, remaining, 3)
        conn = GuardedConnection(self.socket_path, timeout)
        with self._connection_lock:
            self._check_cancel()
            self._active_connection = conn
        return conn

    def _disconnect(self, connection):
        connection.close()
        with self._connection_lock:
            if self._active_connection is connection: self._active_connection = None

    def request_abort(self):
        """Interrupt bounded exports/read barriers before waiting for state ownership."""
        with self._connection_lock:
            # Replayed/concurrent aborts may not interrupt the cleanup they await.
            if self.abort_requested.is_set(): return
            self.abort_requested.set()
            connection = self._active_connection
            if connection is not None and connection.sock is not None:
                try: connection.sock.shutdown(socket.SHUT_RDWR)
                except OSError: pass
        with self._children_lock:
            for child in self._children:
                process = child.process
                if process is not None and process.poll() is None:
                    try: os.killpg(process.pid, signal.SIGTERM)
                    except ProcessLookupError: pass

    def identity(self):
        if process_start(self.pid) != self.start:
            raise RuntimeError("VMM PID/start identity changed")

    def _state(self, state, event):
        self.identity()
        event[state.lower() + "_begin_ns"] = time.monotonic_ns()
        # A lost pause ACK is treated as possibly paused during cleanup.
        if state == "Paused": self.paused = True
        connection = self._connection()
        try:
            connection.request("PATCH", "/vm", json.dumps({"state": state}),
                               {"Content-Type": "application/json"})
            response = connection.getresponse()
            body = response.read(16385)
            if len(body) > 16384:
                raise RuntimeError("VMM state response exceeds client bound")
            if response.status != 204:
                raise RuntimeError(f"VMM {state}: HTTP {response.status}: {body[:512]!r}")
        finally: self._disconnect(connection)
        self.identity()
        self._check_cancel()
        self.paused = state == "Paused"
        event[state.lower() + "_ack_ns"] = time.monotonic_ns()

    def _event(self, kind, action):
        event = {"schema": "aries.vmm-tracer.boundary.v1", "kind": kind,
                 "vm_id": self.vm_id, "vmm_pid": self.pid, "vmm_start_ticks": self.start,
                 "begin_ns": time.monotonic_ns()}
        try:
            return action(event)
        except BaseException as error:
            self.invalid = True
            event["error"] = {"type": type(error).__name__, "message": str(error)}
            raise
        finally:
            try:
                event.update(self._persist_api_calls())
            except BaseException as error:
                self.invalid = True
                event["persistence_error"] = {"type": type(error).__name__, "message": str(error)}
                raise
            finally:
                # Includes receipt persistence, excludes this boundary journal's own write.
                event["end_ns"] = time.monotonic_ns()
                self.events.write(json.dumps(event) + "\n")
                self.events.flush()

    def _persist_api_calls(self):
        """Append each receipt once; do not rewrite an ever-growing history at every boundary."""
        if not self.api.calls:
            return {"api_journal_records": 0, "api_journal_bytes": 0, "api_journal_ns": 0}
        begin = time.monotonic_ns()
        sequence = getattr(self, "_api_call_sequence", 0)
        count = len(self.api.calls)
        data = b"".join((json.dumps({**row, "sequence": sequence + index + 1},
            separators=(",", ":")) + "\n").encode() for index, row in enumerate(self.api.calls))
        with (self.output / "api-calls.jsonl").open("ab") as stream:
            stream.write(data); stream.flush(); os.fsync(stream.fileno())
        self._api_call_sequence = sequence + count
        self.api.calls.clear()
        end = time.monotonic_ns()
        return {"api_journal_records": count, "api_journal_bytes": len(data),
                "api_journal_begin_ns": begin, "api_journal_end_ns": end,
                "api_journal_ns": end - begin}

    def _publish_epoch(self, response, event, censored=False, baseline=False):
        epoch = response["epoch"]
        directory = self.output / ("initial-baseline" if baseline else "epoch-%06d" % epoch)
        event["epoch_publication_begin_ns"] = time.monotonic_ns()
        manifest = self.coordinator.publish(response, directory)
        event["epoch_publication_end_ns"] = time.monotonic_ns()
        record = {"epoch": epoch, "phase_kind": self.phase, "group_id": self.group,
                  "directory": directory.name, "record_id": response["epoch_record_id"],
                  "vm_generation": self.coordinator.generation, "censored": censored,
                  "excluded_from_workload": baseline or self.phase in (None, "bootstrap"),
                  "baseline_initialization": baseline,
                  "gap_access_qualification": "may include observer metadata reads" if self.phase == "llm_wait" else None}
        save(directory / "phase.json", record)
        self.phases.write(json.dumps(record) + "\n")
        self.phases.flush()
        return directory, manifest

    def _seal_pending(self, event):
        if self.pending is None: return None
        event["metadata_barrier_begin_ns"] = time.monotonic_ns()
        self.pending["seal"] = self.pending["child"].await_sealed()
        event["metadata_barrier_end_ns"] = time.monotonic_ns()
        return self.pending

    def _validate_pending(self, response):
        if self.pending is None: return
        pending = self.pending
        child = pending["child"]
        begin = json.loads((child.output / "capture-begin.json").read_text())
        info = self.finalize_capture(self.api, begin, pending["seal"])
        validation = next((v for v in response.get("capture_validation", [])
                           if v["capture_id"] == child.capture_id), None)
        save(child.output / "finalized-info.json", info)
        save(child.output / "validation.json", validation)
        # Retain full overlap evidence before any record acknowledgement.
        overlap = []
        for chunk, _ in self.export_rows(self.api, "/metadata-capture", info["capture_record_id"],
                "dirty_overlap", expected_generation=self.coordinator.generation,
                max_rows=sum(r["page_count"] for r in self.coordinator.regions)):
            overlap.extend(chunk)
        save(child.output / "dirty-overlap.json", overlap)
        pending["info"], pending["begin"] = info, begin
        self.publications.append(pending)
        self.pending = None
        if info["consistency"] != "valid" or validation is None or validation.get("consistency") != "valid":
            raise RuntimeError("metadata capture consistency invalid; retained raw evidence")

    def _finish_publications(self, event):
        event["publication_barrier_begin_ns"] = time.monotonic_ns()
        while self.publications:
            pending = self.publications[0]
            child = pending["child"]
            child.await_publication(timeout=self.config.get("publication_timeout_seconds", 120))
            # Child export manifest and parent verdict are durable before ACK.
            for record in dict.fromkeys((pending["seal"]["capture_record_id"], pending["info"]["capture_record_id"])):
                self.ack_record(self.api, "/metadata-capture", record, self.coordinator.generation)
            self.ack_record(self.api, "/memory-census", pending["begin"]["census_record_id"], self.coordinator.generation)
            self.publications.pop(0)
        event["publication_barrier_end_ns"] = time.monotonic_ns()

    def _rotate(self, next_phase, event, next_group=None, close=False, censored=False):
        self._seal_pending(event)
        event["rotate_begin_ns"] = time.monotonic_ns()
        try:
            response = self.coordinator.rotate(next_phase, close=close)
        except self.RequestError as error:
            save(self.output / "rotation-error.json", error.as_receipt())
            if not error.is_safe_backpressure(self.coordinator.generation) or not self.publications:
                raise
            # One bounded retry only after freeing already finalized, durable records.
            self._finish_publications(event)
            response = self.coordinator.rotate(next_phase, close=close)
        event["rotate_end_ns"] = time.monotonic_ns()
        directory, manifest = self._publish_epoch(response, event, censored)
        self._validate_pending(response)
        self.phase, self.group = next_phase, next_group
        return response, directory, manifest

    def _capture(self, reading_path=None, source=None):
        # Abort publishes its cancellation flag before acquiring this lock.
        # No reader can be created after that flag, including a Popen race.
        with self._children_lock:
            self._check_cancel()
            capture_id = "bootstrap" if reading_path is None else "epoch-%06d" % source["epoch"]
            child = self.Capture(self.companion, self.api, self.socket_path, capture_id,
                 self.coordinator.generation, self.coordinator.epoch, self.output / "captures" / capture_id,
                 timeout_seconds=self.config.get("metadata_barrier_timeout_seconds", 60))
            self.pending = {"child": child}
            self._children.append(child)
            if reading_path is None:
                child.start(config=self.output / "attribution-config.json", vm_id=self.vm_id)
            else:
                child.start(bootstrap=self.bootstrap, reading=reading_path,
                            source_epoch=source["epoch"], source_record_id=source["epoch_record_id"])


    def session_start(self):
        if self.coordinator.generation is not None or self.session_ended:
            raise RuntimeError("duplicate session initialization")
        def action(event):
            self._state("Paused", event)
            self.phase = "bootstrap" if self.attribution_config else "llm_wait"
            response = self.coordinator.arm(self.phase)
            save(self.output / "initial-arm.json", response)
            self._publish_epoch(response, event, baseline=True)
            if self.attribution_config:
                self._capture()
                child = self.pending["child"]
                self._rotate("llm_wait", event)
                self.bootstrap = child.output / "bootstrap.json"
                self._finish_publications(event)
            return {"ok": True, "policy": "pause_during_llm", "initial_gap_epoch": self.coordinator.epoch,
                    "vm_generation": self.coordinator.generation, "paused": self.paused}
        return self._event("session_start", action)

    def arm(self, tool_call_id=None):
        if self.invalid or self.session_ended or self.phase != "llm_wait":
            raise RuntimeError("no valid initial/inter-step paused phase")
        def action(event):
            self._rotate("tool", event, next_group=tool_call_id)
            self._state("Resumed", event)
            return {"arm_begin_ns": event["begin_ns"], "arm_end_ns": time.monotonic_ns(), "epoch": self.coordinator.epoch,
                    "vm_generation": self.coordinator.generation}
        return self._event("arm", action)

    def _reading(self, response, directory):
        self._check_cancel()
        masks = {name: set() for name in ("access_observed", "access_unknown", "access_idle",
                                         "kvm_dirty", "vmm_dirty", "combined_dirty")}
        unknown = {}
        access_sets = tuple(masks["access_" + name] for name in ("observed", "unknown", "idle"))
        def checked_gpa(row):
            gpa = row["gpa"]
            if (type(gpa) is not int or gpa < 0 or gpa % 4096 or not any(
                    r["guest_base"] <= gpa < r["guest_base"] + r["bytes"] for r in self.coordinator.regions)):
                raise ValueError("exported GPA is outside authoritative RAM")
            return gpa
        for ordinal, row in enumerate(rows(directory / "access.jsonl.gz")):
            if ordinal % 4096 == 0: self._check_cancel()
            gpa = checked_gpa(row)
            if any(gpa in selected for selected in access_sets) or row["state"] not in ("observed", "unknown", "idle"):
                raise ValueError("duplicate or invalid exported access classification")
            masks["access_" + row["state"]].add(gpa)
            if row["state"] == "unknown":
                reason = row["unknown_reason"]
                if not isinstance(reason, str) or not reason.replace("_", "").replace("-", "").isalnum():
                    raise ValueError("unexpected unknown reason label")
                masks.setdefault("unknown_" + reason, set()).add(row["gpa"])
                unknown[str(row["gpa"])] = reason
        for ordinal, row in enumerate(rows(directory / "dirty.jsonl.gz")):
            if ordinal % 4096 == 0: self._check_cancel()
            gpa = checked_gpa(row)
            if (gpa in masks["combined_dirty"] or any(type(row[name]) is not bool for name in ("kvm_dirty", "vmm_dirty"))
                    or not (row["kvm_dirty"] or row["vmm_dirty"])):
                raise ValueError("duplicate or invalid exported dirty provenance")
            for name in ("kvm_dirty", "vmm_dirty"):
                if row[name]: masks[name].add(gpa)
            masks["combined_dirty"].add(gpa)
        regions = []
        for index, region in enumerate(self.coordinator.regions):
            region = dict(region, masks={})
            buffers = {}
            for name, selected in masks.items():
                self._check_cancel()
                buffer = bytearray(((region["page_count"] + 63) // 64) * 8)
                for ordinal, gpa in enumerate(selected):
                    if ordinal % 4096 == 0: self._check_cancel()
                    offset = gpa - region["guest_base"]
                    if 0 <= offset < region["bytes"]:
                        bit = offset // 4096
                        buffer[bit // 8] |= 1 << (bit % 8)
                buffers[name] = buffer
            # Native AccessRecord::rows omits pages absent at all finite samples.
            # Keep that complement distinct from the final census's residency.
            classified = (int.from_bytes(buffers["access_observed"], "little") |
                          int.from_bytes(buffers["access_unknown"], "little") |
                          int.from_bytes(buffers["access_idle"], "little"))
            absent = ((1 << region["page_count"]) - 1) ^ classified
            buffers["access_absent"] = absent.to_bytes(len(buffers["access_observed"]), "little")
            for name, buffer in buffers.items():
                self._check_cancel()
                path = directory / f"region-{index}-{name}.bin"
                with path.open("xb") as stream:
                    stream.write(buffer); stream.flush(); os.fsync(stream.fileno())
                region["masks"][name] = {"file": path.name, "bytes": len(buffer),
                    "sha256": hashlib.sha256(buffer).hexdigest(),
                    "pages": sum(b.bit_count() for b in buffer)}
            regions.append(region)
        reading = {"schema": "aries.vmm-tracer.compact.v1", "page_bytes": 4096,
            "vm_instance": self.vm_id, "vmm_pid": self.pid, "tool_call_id": self.current_group,
            "group_id": self.current_group, "epoch": response["epoch"], "phase_kind": "tool",
            "vm_generation": self.coordinator.generation, "regions": regions,
            "source_epoch": response["epoch"], "source_record_id": response["epoch_record_id"],
            "capture_epoch": self.coordinator.epoch, "artifact_directory": str(directory),
            "observed_pages": len(masks["access_observed"]), "unknown_pages": len(unknown),
            "dirty_pages": len(masks["combined_dirty"]),
            "counted_pages": len(masks["access_observed"] | masks["access_unknown"] | masks["combined_dirty"]),
            "batch_uncertainty": ["host page-idle remains a qualified access signal",
                "VMM dirty denotes potential writes and can conservatively overcount",
                "read-only observer accesses contaminate paused-gap access masks"],
            "device_quiescence": "HTTP-held pause and checked asynchronous block drain",
            "counted_policy": "access_observed union access_unknown union conservative combined_dirty"}
        if self.bootstrap:
            bootstrap = json.loads(self.bootstrap.read_text())
            reading["layout_identity"] = bootstrap["capture_binding"]["layout_identity"]
        self._check_cancel()
        save(directory / "reading.json", reading)
        return reading

    def collect(self):
        if self.invalid or self.phase != "tool":
            raise RuntimeError("no valid tool phase")
        def action(event):
            self.current_group = self.group
            self._state("Paused", event)
            response, directory, _ = self._rotate("llm_wait", event)
            self._finish_publications(event)
            reading = self._reading(response, directory)
            if self.attribution_config:
                self._capture(directory / "reading.json", response)
            return reading
        return self._event("collect", action)

    def _cleanup_capture(self):
        if self.pending is not None:
            child = self.pending["child"]
            info = self.api.call("/metadata-capture", "info", capture_id=child.capture_id,
                                 vm_generation=self.coordinator.generation)
            if (info.get("schema") != "aries.firecracker.metadata-capture.v1" or
                info.get("vm_generation") != self.coordinator.generation or
                info.get("capture_id") != child.capture_id or info.get("epoch") != child.epoch):
                raise RuntimeError("cannot establish capture state for cleanup")
            if info.get("state") == "sealed" and info.get("consistency") in ("valid", "invalid", "quiescence_only"):
                # A completed rotation already closed/validated live bytes. Failure
                # to export the verdict is publication failure, not grounds to
                # discard finalized server evidence.
                child.sealed = True
                receipt = child.abort("cleanup after finalized capture publication failure")
                receipt = {**receipt, "finalized_record_retained": True, "info": info}
            else:
                receipt = child.discard_for_cleanup("observer failed/closing; no valid attribution claim")
            if receipt.get("reader_process_reaped") is not True:
                raise RuntimeError("capture reader termination unconfirmed")
            save(self.output / "cleanup-capture.json", receipt)
            self.pending = None

    def abort_session(self, reason="host_confirmed_harness_stopped"):
        """Release isolation separately from measurement validity, without a final scan."""
        if getattr(self, "release_confirmed", False):
            return {"ok": True, "released": True, "paused": False,
                    "already_ended": True, "measurement_valid": not self.invalid,
                    "terminal_complete": getattr(self, "terminal_complete", False)}
        self._cleanup_mode = True
        self._cleanup_deadline = time.monotonic() + 20
        def action(event):
            event["reason"] = reason
            prior_complete = self.session_ended and getattr(self, "terminal_complete", False)
            if not prior_complete:
                self.invalid = True
                event["censored_epoch"] = self.coordinator.epoch
                event["censored_phase_kind"] = self.phase
                event["censored_group_id"] = self.group
                event["measurement_complete"] = False
                event["epoch_collection_skipped"] = True
                event["coordinator_left_armed_until_vm_cleanup"] = self.coordinator.epoch is not None
                # Kill/reap the reader and explicitly resolve its VMM live-read
                # ownership. A failed or ambiguous closure forbids Resume.
                self._cleanup_capture()
            for pending in list(self.publications):
                if time.monotonic() >= self._cleanup_deadline:
                    raise TimeoutError("observer cancellation cleanup budget exhausted")
                child = pending["child"]
                pending["cancellation_cleanup"] = child.abort(reason)
                if pending["cancellation_cleanup"].get("reader_process_reaped") is not True:
                    raise RuntimeError("publication child termination unconfirmed")
            # Never trust a local paused flag after an ambiguous HTTP outcome.
            self.identity()
            connection = self._connection()
            try:
                connection.request("GET", "/")
                response = connection.getresponse()
                raw = response.read(16385)
                if response.status != 200 or len(raw) > 16384:
                    raise RuntimeError("VMM state cannot be confirmed")
                state = json.loads(raw).get("state")
                if state not in ("Paused", "Running"):
                    raise RuntimeError("VMM state is neither Paused nor Running")
            finally: self._disconnect(connection)
            self.identity()
            event["state_before_release"] = state
            if state == "Paused": self._state("Resumed", event)
            else:
                self.paused = False
                event["running_confirmed_ns"] = time.monotonic_ns()
            self.session_ended = self.release_confirmed = True
            self.terminal_complete = bool(prior_complete)
            event["released"] = True
            return {"ok": True, "released": True, "paused": False,
                    "measurement_valid": not self.invalid,
                    "terminal_complete": self.terminal_complete}
        try: return self._event("session_abort", action)
        finally: self._cleanup_mode = False

    def session_end(self, reason="native_terminal", terminal_complete=False):
        if self.session_ended:
            return {"ok": not self.invalid, "already_ended": True, "paused": self.paused,
                    "released": getattr(self, "release_confirmed", False),
                    "terminal_complete": getattr(self, "terminal_complete", False)}
        complete_gap = terminal_complete and self.phase == "llm_wait"
        def action(event):
            try:
                if self.coordinator.epoch is not None and not self.invalid:
                    if not self.paused: self._state("Paused", event)
                    self._rotate(None, event, close=True,
                                 censored=not complete_gap)
                else:
                    self._cleanup_capture()
            except BaseException:
                self.invalid = True
                self._cleanup_capture()
                raise
            finally:
                # Only after all child live reads are closed/discarded.
                if self.pending is None and self.paused:
                    self._state("Resumed", event)
                self.session_ended = True
            self._finish_publications(event)
            self.terminal_complete = complete_gap and not self.invalid
            self.release_confirmed = not self.paused
            return {"ok": not self.invalid, "paused": self.paused, "reason": reason,
                    "released": self.release_confirmed,
                    "terminal_complete": self.terminal_complete}
        return self._event("session_end", action)

    def close(self):
        if self.closed: return
        failure = None
        try:
            if not self.session_ended: self.session_end("observer_shutdown", False)
        except BaseException as error:
            failure = error
        finally:
            cleanup_errors = []
            # Always reap publication children, including when session_end failed.
            # These children already sealed their live reads: abort() only stops
            # local publication and retains valid frozen server records.
            for pending in self.publications:
                child = pending["child"]
                if child.process is not None and child.process.poll() is None:
                    try:
                        child.abort("observer shutdown after sealed publication failure")
                    except BaseException as error:
                        cleanup_errors.append({"type": type(error).__name__, "message": str(error)})
            if cleanup_errors:
                save(self.output / "publication-cleanup-errors.json", cleanup_errors)
                if failure is None: failure = RuntimeError("publication child cleanup failed")
            try:
                self._persist_api_calls()
            except BaseException as error:
                self.invalid = True
                if failure is None: failure = error
            self.events.close(); self.phases.close(); self.closed = True
        if failure is not None: raise failure
