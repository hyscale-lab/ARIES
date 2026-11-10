"""Continuous access/dirty epochs with the user-selected paused-LLM baseline."""
import base64
import os
import array
import gzip
import sys
import hashlib
import json
import time
import threading
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tsb-attribution"))
from paused import PausedAttributionObserver, UnixHTTPConnection

PAGE = 4096
BIT_OFFSETS = tuple(tuple(bit for bit in range(8) if byte & (1 << bit)) for byte in range(256))

def bitmap_pages(raw, guest_base):
    for index, byte in enumerate(raw):
        for bit in BIT_OFFSETS[byte]:
            yield guest_base + (index * 8 + bit) * PAGE


def pack_pages(pages, regions):
    buffers = [bytearray(((r["memory_size"] // PAGE + 63) // 64) * 8) for r in regions]
    for g in pages:
        if not isinstance(g, int) or g < 0 or g % PAGE:
            raise ValueError("unaligned guest page")
        for r, buf in zip(regions, buffers):
            offset = g - r["guest_phys_addr"]
            if 0 <= offset < r["memory_size"]:
                bit = offset // PAGE
                buf[bit // 8] |= 1 << (bit % 8)
                break
        else:
            raise ValueError("guest page outside RAM slots")
    return [bytes(b) for b in buffers]


def decode_bitmap(value, pages):
    raw = base64.b64decode(value, validate=True)
    if len(raw) != ((pages + 63) // 64) * 8:
        raise ValueError("dirty bitmap length mismatch")
    integer = int.from_bytes(raw, "little")
    if integer >> pages:
        raise ValueError("dirty bitmap has nonzero padding")
    return raw, integer


def validate_dirty(result, epoch, next_epoch, slots):
    if result.get("schema") != "aries.firecracker.dirty.v1":
        raise ValueError("unsupported dirty response schema")
    if result.get("epoch") != epoch or result.get("next_epoch") != next_epoch:
        raise ValueError("dirty epoch mismatch")
    if result.get("page_bytes") != PAGE:
        raise ValueError("dirty page size mismatch")
    regions = result.get("regions", [])
    expected = {(s["guest_phys_addr"], s["memory_size"]) for s in slots}
    actual = {(r["guest_base"], r["bytes"]) for r in regions}
    if len(regions) != len(slots) or actual != expected:
        raise ValueError("dirty RAM regions differ from captured KVM slots")
    for r in regions:
        pages = r["bytes"] // PAGE
        if r["page_count"] != pages or r["bytes"] % PAGE:
            raise ValueError("dirty region page count mismatch")
        bits = {}
        for source in ("kvm", "vmm", "combined"):
            _, bits[source] = decode_bitmap(r[source + "_dirty_b64"], pages)
            if bits[source].bit_count() != r[source + "_dirty_pages"]:
                raise ValueError("dirty bitmap population mismatch")
        if bits["combined"] != bits["kvm"] | bits["vmm"]:
            raise ValueError("dirty union mismatch")
    return result


class DirtyClient:
    def __init__(self, socket_path, timeout=10):
        self.socket_path, self.timeout = socket_path, timeout

    def request(self, operation, epoch, next_epoch=None):
        payload = {"operation": operation, "epoch": epoch}
        if next_epoch is not None:
            payload["next_epoch"] = next_epoch
        conn = UnixHTTPConnection(self.socket_path, self.timeout)
        try:
            conn.request("PUT", "/dirty-log", json.dumps(payload), {"Content-Type": "application/json"})
            response = conn.getresponse()
            body = response.read()
            if response.status != 200:
                raise RuntimeError(f"dirty {operation}: HTTP {response.status}: {body[:512]!r}")
            return json.loads(body)
        finally:
            conn.close()


class AccessDirtyObserver(PausedAttributionObserver):
    """One initial arm, then atomic dirty rotations across tool and LLM_wait phases."""
    def __init__(self, observer, config, output, dirty_client=None, decoder=None):
        self.base = observer
        self.pid, self.vm_id = observer.pid, observer.vm_id
        self.slots, self.start = observer.slots, observer.start
        self.config = dict(config)
        self.output = Path(output)
        self.output.mkdir(parents=True, exist_ok=False)
        self.paused = False
        self.socket_path = self.resolve_socket()
        self.client = dirty_client or DirtyClient(self.socket_path, config.get("api_timeout_seconds", 10))
        self.decoder = decoder
        self.bootstrapped = False
        if decoder is None and config.get("attribution"):
            from decoder import GuestDecoder
            self.decoder = GuestDecoder(self.pid, self.vm_id, self.slots, config["attribution"])
        self.events = (self.output / "boundaries.jsonl").open("x")
        self.phases = (self.output / "phases.jsonl").open("x")
        self.active = None
        self.epoch = 0
        self.invalid = False
        self.closed = False
        self.pending_publication = []
        self.executor = ThreadPoolExecutor(max_workers=1, thread_name_prefix='attribution')
        self.metadata_bundle = None
        self.session_ended = False
        if config.get('attribution', {}).get('enable_compression'):
            raise ValueError('payload compression is outside this protocol')

    def _timed(self, event, name, fn):
        event[name + "_begin_ns"] = time.monotonic_ns()
        try:
            return fn()
        finally:
            event[name + "_end_ns"] = time.monotonic_ns()

    def _census(self):
        resident, absent = self.base.inventory()
        try:
            values = {}
            for line in Path(f"/proc/{self.pid}/smaps_rollup").read_text().splitlines():
                parts = line.split()
                if len(parts) == 3 and parts[2] == "kB":
                    values[parts[0].rstrip(":")] = int(parts[1]) * 1024
            memory = {"available": "Rss" in values and "Pss" in values, "bytes": values}
        except OSError as error:
            memory = {"available": False, "error_type": type(error).__name__}
        return {"resident_guest_pages": len(resident),
                "distinct_nonzero_host_pfns": len({p for p in resident.values() if p}),
                "zero_host_pfn_pages": sum(p == 0 for p in resident.values()),
                "absent_gpa_ranges": absent,
                "resident_gpas": list(resident), "resident_pfns": resident, "vmm_memory": memory}

    def _start_access(self, kind, group_id, event):
        arm = self._timed(event, "access_arm", lambda: self.base.arm(group_id))
        self.active = {"epoch": self.epoch, "phase_kind": kind, "group_id": group_id,
                       "arm": arm, "resident_at_arm": self._timed(event, "resident_arm", self._census)}
        return arm

    def _finish(self, event, next_kind=None, next_group=None, censored=False):
        phase = self.active
        metadata = None
        observer_reads = []
        if phase["phase_kind"] == "LLM_wait" and self.metadata_bundle is not None:
            pending = self.metadata_bundle
            try:
                metadata = self._timed(event, "metadata_barrier",
                    lambda: pending["attribution_future"].result(timeout=self.config.get("metadata_barrier_timeout_seconds", 60)))
            except Exception as error:
                pending["attribution_error"] = {"type": type(error).__name__, "message": str(error)}
                if not pending["metadata_reads_done"].is_set():
                    self.invalid = True
                    pending["metadata_abort"].set()
                    # Never resume or start another epoch while a live reader remains.
                    self._cancel_reader(pending)
            observer_reads = list(pending.get("metadata_read_gpas", []))
            self.metadata_bundle = None
        reading = self._timed(event, "access_collect", self.base.collect)
        next_epoch = self.epoch + 1 if next_kind else None
        dirty = self._timed(event, "dirty_collect",
                            lambda: self.client.request("collect", self.epoch, next_epoch))
        validate_dirty(dirty, self.epoch, next_epoch, self.slots)
        if phase["phase_kind"] == "LLM_wait" and 'pending' in locals():
            changed = []
            if observer_reads:
                pages = observer_reads
                for region in dirty["regions"]:
                    _, bits = decode_bitmap(region["combined_dirty_b64"], region["page_count"])
                    for g in pages:
                        bit = (g - region["guest_base"]) // PAGE
                        if 0 <= bit < region["page_count"] and bits & (1 << bit):
                            changed.append(g)
            consistency = {"valid": metadata is not None and not changed,
                           "changed_metadata_gpas": changed,
                           "scope": "no tracked CPU/VMM writes to captured metadata during entire tool-end-to-barrier gap; covered writers only",
                           "metadata_begin_ns": pending.get("metadata_begin_ns"),
                           "metadata_end_ns": pending.get("metadata_end_ns"),
                           "validation_epoch": self.epoch,
                           "error": pending.get("attribution_error")}
            target = self._directory(pending["phase"]) / "attribution-consistency.json"
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(json.dumps(consistency, indent=2) + "\n")
        census = self._timed(event, "resident_collect", self._census)
        # Raw evidence remains publishable if optional metadata discovery fails.
        bundle = {"phase": phase, "reading": reading, "dirty": dirty, "resident_at_collect": census,
                  "censored": censored, "boundary": event, "attribution": None,
                  "observer_metadata_read_gpas": observer_reads}
        self.pending_publication.append(bundle)
        self.active = None
        if next_kind and not self.invalid:
            self.epoch = next_epoch
            self._start_access(next_kind, next_group, event)
        if self.decoder is not None and phase["phase_kind"] == "tool":
            self.metadata_bundle = bundle
            bundle["metadata_reads_done"] = threading.Event()
            bundle["metadata_abort"] = threading.Event()
            self.decoder.capture_abort = bundle["metadata_abort"]
            dirty_sources = [(region["guest_base"], region["page_count"],
                              region["kvm_dirty_b64"], region["vmm_dirty_b64"]) for region in dirty["regions"]]
            def discovery():
                bundle["metadata_begin_ns"] = time.monotonic_ns()
                self.decoder.metadata_read_gpas = set()
                try:
                    additional = {}
                    for base, count, kvm, vmm in dirty_sources:
                        for source, encoded in (("kvm_dirty", kvm), ("vmm_potential_write", vmm)):
                            raw, _ = decode_bitmap(encoded, count)
                            for gpa in bitmap_pages(raw, base):
                                if bundle["metadata_abort"].is_set():
                                    raise RuntimeError("metadata preparation canceled")
                                additional.setdefault(str(gpa), []).append(source)
                    effective_reading = dict(reading)
                    effective_reading["additional_gpas"] = [int(g) for g in additional]
                    effective_reading["additional_access_sources"] = additional
                    data = self.decoder.capture_metadata(effective_reading)
                finally:
                    bundle["metadata_end_ns"] = time.monotonic_ns()
                    bundle["metadata_read_gpas"] = sorted(self.decoder.metadata_read_gpas)
                    bundle["metadata_reads_done"].set()
                result = self.decoder.publish_capture(data, self._directory(phase) / "attribution")
                return data, result
            bundle["attribution_future"] = self.executor.submit(discovery)
        return bundle

    def _directory(self, phase):
        return self.output / f"epoch-{phase['epoch']:06d}-{phase['phase_kind']}"

    def _publish(self, bundle):
        phase, reading, dirty = bundle["phase"], bundle["reading"], bundle["dirty"]
        directory = self._directory(phase)
        directory.mkdir(exist_ok=True)
        manifests = []
        masks = {"access_observed": reading["accessed_gpas"],
                 "access_idle": reading["idle_gpas"],
                 "access_unknown": [int(g) for g in reading["unknown_gpas"]],
                 "resident_arm": phase["resident_at_arm"]["resident_gpas"],
                 "resident_collect": bundle["resident_at_collect"]["resident_gpas"]}
        masks["observer_metadata_read"] = bundle["observer_metadata_read_gpas"]
        for reason in sorted(set(reading["unknown_gpas"].values())):
            masks["unknown_" + reason] = [int(g) for g, why in reading["unknown_gpas"].items() if why == reason]
        packed = {name: pack_pages(pages, self.slots) for name, pages in masks.items()}
        by_region = {r["guest_base"]: r for r in dirty["regions"]}
        for index, slot in enumerate(self.slots):
            record = {"guest_base": slot["guest_phys_addr"], "bytes": slot["memory_size"], "masks": {}}
            source = by_region[slot["guest_phys_addr"]]
            for name in ("kvm", "vmm", "combined"):
                packed[name + "_dirty"] = packed.get(name + "_dirty", [])
                raw, _ = decode_bitmap(source[name + "_dirty_b64"], slot["memory_size"] // PAGE)
                packed[name + "_dirty"].append(raw)
            idle_union = int.from_bytes(packed["access_observed"][index], "little") | int.from_bytes(packed["access_unknown"][index], "little")
            dirt = int.from_bytes(packed["combined_dirty"][index], "little")
            for label, bits in (("effective_touched", idle_union | dirt), ("dirty_outside_idle_counted", dirt & ~idle_union), ("observer_read_overlap_access", int.from_bytes(packed["observer_metadata_read"][index], "little") & int.from_bytes(packed["access_observed"][index], "little"))):
                packed.setdefault(label, []).append(bits.to_bytes(len(packed["combined_dirty"][index]), "little"))
            for name, buffers in packed.items():
                raw = buffers[index]
                path = directory / f"region-{index:03d}-{name}.bin"
                path.write_bytes(raw)
                record["masks"][name] = {"file": path.name, "bytes": len(raw),
                                         "pages": int.from_bytes(raw, "little").bit_count(),
                                         "sha256": hashlib.sha256(raw).hexdigest()}
            record["host_pfn_maps"] = {}
            for point, census in (("arm", phase["resident_at_arm"]), ("collect", bundle["resident_at_collect"])):
                pfns = array.array("Q", [0]) * (slot["memory_size"] // PAGE)
                for gpa, pfn in census["resident_pfns"].items():
                    off = gpa - slot["guest_phys_addr"]
                    if 0 <= off < slot["memory_size"]:
                        pfns[off // PAGE] = pfn
                if sys.byteorder != "little":
                    pfns.byteswap()
                raw = pfns.tobytes()
                path = directory / f"region-{index:03d}-host-pfns-{point}.u64le.gz"
                with gzip.open(path, "wb", compresslevel=1) as stream:
                    stream.write(raw)
                record["host_pfn_maps"][point] = {"file": path.name,
                    "encoding": "gzip of little-endian uint64 PFNs, indexed by region guest page",
                    "raw_bytes": len(raw), "raw_sha256": hashlib.sha256(raw).hexdigest(),
                    "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                    "resident_mask": "resident_" + point,
                    "zero_semantics": "absent iff resident mask is zero; resident PFN zero is unresolved"}
            manifests.append(record)
        for region in dirty["regions"]:
            for name in ("kvm", "vmm", "combined"):
                region.pop(name + "_dirty_b64")
        compact = {k: v for k, v in reading.items() if k not in ("accessed_gpas", "idle_gpas", "unknown_gpas")}
        compact.update({"schema": "aries.access-dirty.phase.v1", "epoch": phase["epoch"],
                        "phase_kind": phase["phase_kind"], "group_id": phase["group_id"],
                        "censored": bundle["censored"], "dirty": dirty, "regions": manifests,
                        "observed_pages": len(reading["accessed_gpas"]),
                        "unknown_pages": len(reading["unknown_gpas"]),
                        "idle_pages": len(reading["idle_gpas"]),
                        "artifact_directory": str(directory),
                        "measurement_valid": not self.invalid,
                        "logical_epoch_only": True,
                        "gap_access_scope": "raw gap access includes observer metadata reads; overlap mask is not subtracted and cannot separate coincident device access",
                        "dirty_scope": "KVM write evidence plus conservative VMM writable-buffer exposure; VMM bits need not prove completed writes",
                        "epoch_scope": "sequential paused access and dirty boundaries; physical skew retained",
                        "resident_at_arm": {k: v for k, v in phase["resident_at_arm"].items() if k not in ("resident_gpas", "resident_pfns")},
                        "resident_at_collect": {k: v for k, v in bundle["resident_at_collect"].items() if k not in ("resident_gpas", "resident_pfns")}})
        if bundle.get("attribution_future") is not None:
            compact["attribution_pending"] = True
            compact["attribution_consistency_file"] = "attribution-consistency.json"
        if bundle.get("attribution_error"):
            compact["attribution_error"] = bundle["attribution_error"]
        compact["boundary_pause"] = dict(bundle["boundary"])
        (directory / "phase.json").write_text(json.dumps(compact, indent=2) + "\n")
        self.phases.write(json.dumps(compact) + "\n")
        self.phases.flush()
        return compact

    def _transition(self, kind, action):
        result = None
        try:
            result, boundary = self._boundary(kind, action)
        except BaseException:
            self.invalid = True
            raise
        finally:
            # Raw publication is synchronous before acknowledgement; P2 discovery/publication
            # overlaps the user-selected held-VM LLM phase.
            if not self.paused or (result is not None and kind == "collect"):
                pending, self.pending_publication = self.pending_publication, []
                try:
                    for bundle in pending:
                        begin = time.monotonic_ns()
                        bundle["published"] = self._publish(bundle)
                        self.events.write(json.dumps({"kind": "publication", "epoch": bundle["phase"]["epoch"],
                                                      "begin_ns": begin, "end_ns": time.monotonic_ns()}) + "\n")
                        self.events.flush()
                except BaseException as error:
                    self.invalid = True
                    if self.paused:
                        self._state("Resumed")
                        self.paused = False
                    self.events.write(json.dumps({"kind": "publication_error",
                        "time_ns": time.monotonic_ns(), "error_type": type(error).__name__}) + "\n")
                    self.events.flush()
                    raise
        return result, boundary

    def _cancel_reader(self, pending):
        pending["metadata_abort"].set()
        begin = time.monotonic_ns()
        if pending["metadata_reads_done"].wait(self.config.get("metadata_abort_grace_seconds", 5)):
            return
        # Exit the whole process: no Python reader may survive controller release.
        self.invalid = True
        receipt = {"schema": "aries.access-dirty.fatal-reader.v1", "observer_pid": os.getpid(),
                   "vmm_pid": self.pid, "vmm_start_ticks": self.start, "vm_id": self.vm_id,
                   "socket_path": self.socket_path, "cancel_begin_ns": begin,
                   "fatal_ns": time.monotonic_ns(), "reason": "metadata reader did not stop",
                   "measurement_valid": False,
                   "partial_metadata_read_gpas": sorted(getattr(self.decoder, "metadata_read_gpas", set()))}
        target = self.output / "fatal-reader.json"
        with target.open("w") as stream:
            json.dump(receipt, stream); stream.flush(); os.fsync(stream.fileno())
        self.events.flush(); self.phases.flush()
        os._exit(86)

    def _state(self, state):
        if state == "Resumed" and self.metadata_bundle is not None:
            pending = self.metadata_bundle
            if not pending["metadata_reads_done"].is_set():
                pending["metadata_abort"].set()
                self._cancel_reader(pending)
        return super()._state(state)

    def _boundary(self, kind, action):
        event = {"schema": "aries.access-dirty.boundary.v1", "kind": kind,
                 "vm_id": self.vm_id, "vmm_pid": self.pid, "vmm_start_ticks": self.start,
                 "begin_ns": time.monotonic_ns(), "already_policy_paused": self.paused}
        success = False
        try:
            if not self.paused:
                self.paused = True
                self._state("Paused")
            event["pause_ack_ns"] = time.monotonic_ns()
            result = action(event)
            event["capture_end_ns"] = time.monotonic_ns()
            success = True
            return result, event
        except BaseException as error:
            event["error"] = {"type": type(error).__name__, "message": str(error)}
            raise
        finally:
            try:
                if not success or kind not in ("collect", "session_start"):
                    self._state("Resumed")
                    self.paused = False
                    event["resume_ack_ns"] = time.monotonic_ns()
                else:
                    event["policy_hold_begin_ns"] = time.monotonic_ns()
                    event["hold_reason"] = "user-selected pause during LLM processing"
            except BaseException as error:
                event["resume_error"] = {"type": type(error).__name__, "message": str(error)}
                raise
            finally:
                event["end_ns"] = time.monotonic_ns()
                self.events.write(json.dumps(event) + "\n")
                self.events.flush()

    def _initial_dirty(self):
        result = self.client.request("arm", 1)
        validate_dirty(result, 1, None, self.slots)
        if result.get("active_epoch") != 1 or result.get("boot_baseline_discarded") is not True or result.get("vcpu_paused") is not True:
            raise ValueError("dirty initialization did not confirm paused active epoch and discarded baseline")
        self._save("initial-dirty-arm.json", result)
        return result

    def session_start(self):
        if self.active or self.epoch or self.session_ended:
            raise RuntimeError("duplicate session initialization")
        def action(event):
            if self.decoder is not None:
                self.bootstrap_data = self._timed(event, "bootstrap", self.decoder.bootstrap)
                self.bootstrapped = True
            self.epoch = 1
            self._timed(event, "dirty_initial_arm", self._initial_dirty)
            return self._start_access("LLM_wait", None, event)
        arm, boundary = self._transition("session_start", action)
        if hasattr(self, "bootstrap_data"):
            self._save("bootstrap.json", self.bootstrap_data)
            del self.bootstrap_data
        return {"ok": True, "policy": "pause_during_llm", "initial_gap_epoch": 1, "boundary": boundary}

    def session_end(self, reason="native_terminal", terminal_complete=False):
        if self.session_ended:
            return {"ok": True, "already_ended": True, "paused": self.paused}
        try:
            if self.active and not self.invalid:
                self._transition("session_end", lambda event: self._finish(event, censored=not (terminal_complete and self.active["phase_kind"] == "LLM_wait")))
            elif self.paused:
                self._state("Resumed")
                self.paused = False
        finally:
            self.session_ended = True
        return {"ok": True, "paused": self.paused, "reason": reason}

    def arm(self, tool_call_id=None):
        if self.invalid or self.session_ended:
            raise RuntimeError("observer invalid or session ended")
        if not self.epoch:
            raise RuntimeError("initial session pause required before first tool batch")
        if self.active and self.active["phase_kind"] != "LLM_wait":
            raise RuntimeError("tool epoch already active")
        def action(event):
            if self.decoder is not None and not self.bootstrapped:
                bootstrap = self._timed(event, "bootstrap", self.decoder.bootstrap)
                self.bootstrapped = True
                self.bootstrap_data = bootstrap
            if self.active:
                self._finish(event, "tool", tool_call_id)
                if self.invalid or self.active is None:
                    raise RuntimeError("metadata barrier failed; next epoch invalid")
                return self.active["arm"]
            self.epoch = 1
            self._timed(event, "dirty_initial_arm", self._initial_dirty)
            return self._start_access("tool", tool_call_id, event)
        result, boundary = self._transition("arm", action)
        if hasattr(self, "bootstrap_data"):
            self._save("bootstrap.json", self.bootstrap_data)
            del self.bootstrap_data
        return dict(result, boundary_pause=boundary, epoch=self.epoch)

    def collect(self):
        if self.invalid or not self.active or self.active["phase_kind"] != "tool":
            raise RuntimeError("no valid tool epoch to collect")
        bundle, boundary = self._transition("collect", lambda event: self._finish(event, "LLM_wait"))
        return bundle["published"]

    def close(self):
        if self.closed:
            return
        try:
            if self.active and not self.invalid:
                self._transition("close", lambda event: self._finish(event, censored=True))
            elif self.paused:
                self._state("Resumed")
                self.paused = False
        finally:
            self.executor.shutdown(wait=True)
            try:
                if self.decoder is not None:
                    self.decoder.close()
            finally:
                self.base.close()
                self.events.close()
                self.phases.close()
                self.closed = True
