#!/usr/bin/env python3
"""Run controlled target tests using one guest-fixture process and one host timeline.
Example: sudo python3 calibrate.py --pid PID --vm-id ID --slots slots.json
  --output runs/.../calibration.jsonl --guest-command EXECUTABLE ARG...
The executable must provide persistent pipes to guest-fixture as guest root.
"""
import argparse
import hashlib
import resource
import json
import os
import selectors
import signal
import socket
import http.client
import threading
from capture_slots import parse_slot
import subprocess
import time
from pathlib import Path
from idle import IdleObserver


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--pid", type=int)
    ap.add_argument("--trace-parent", type=int, help="capture KVM slots for a newly launched driver VM")
    ap.add_argument("--vm-id", required=True)
    ap.add_argument("--slots")
    ap.add_argument("--output", required=True)
    ap.add_argument("--extended", action="store_true")
    ap.add_argument("--diagnose-failed-target", action="store_true", help="run remaining diagnostics after a failed target gate without admitting benchmarks")
    ap.add_argument("--vm-info", help="driver vm-info.json to verify exact machine configuration")
    ap.add_argument("--guest-command", nargs=argparse.REMAINDER, required=True)
    args = ap.parse_args()
    gate_path = Path(__file__).with_name("quality-gate.json")
    gate = json.loads(gate_path.read_text())
    # Reserve the artifact before any runtime effects; never overwrite a run.
    out = open(args.output, "x")
    def cancelled(signum, frame):
        raise KeyboardInterrupt(f"signal {signum}")
    signal.signal(signal.SIGTERM, cancelled)
    slot_events = []
    tracer = None
    if args.trace_parent:
        tracer = subprocess.Popen(["strace", "-f", "-e", "trace=ioctl", "-p", str(args.trace_parent)],
                                  stderr=subprocess.PIPE, stdout=subprocess.DEVNULL, text=True)
        def trace_reader():
            for line in tracer.stderr:
                e = parse_slot(line)
                if e:
                    slot_events.append(e)
        thread = threading.Thread(target=trace_reader)
        thread.start()
        time.sleep(1)
        if tracer.poll() is not None:
            raise RuntimeError("strace failed to attach")
    proc = subprocess.Popen(args.guest_command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            text=True, bufsize=1)
    selector = selectors.DefaultSelector()
    selector.register(proc.stdout, selectors.EVENT_READ)
    observer = None
    # One line per pipe read avoids buffered-read/select disagreement.
    buffer = bytearray()
    def event():
        deadline = time.monotonic() + 60
        while b"\n" not in buffer:
            if not selector.select(max(0, deadline - time.monotonic())):
                raise TimeoutError("guest fixture event timeout")
            chunk = os.read(proc.stdout.fileno(), 65536)
            if not chunk:
                raise RuntimeError("guest fixture pipe ended")
            buffer.extend(chunk)
        line, _, remainder = buffer.partition(b"\n")
        buffer[:] = remainder
        return json.loads(line)
    def send(command):
        proc.stdin.write(command + "\n")
        proc.stdin.flush()
    def until(kind):
        events = []
        while True:
            e = event()
            events.append(e)
            if e["event"] == kind:
                return events
    records = []
    with out:
        def save(e):
            out.write(json.dumps(e, separators=(",", ":")) + "\n")
            out.flush()
        save({"schema": "aries.tsb.calibration-manifest.v1", "gate": gate,
              "host_monotonic_ns": time.monotonic_ns(), "scope": "target calibration, not whole guest",
              "source_sha256": {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in Path(__file__).parent.iterdir() if p.suffix in (".py", ".c", ".json")},
              "target_gate_additional_criterion": "zero unknown target pages, encoded before first target measurements; strict diagnostic identity requirement",
              "guest_command": args.guest_command})
        try:
            events = until("ready")
            for e in events:
                save(e)
            if tracer:
                tracer.send_signal(signal.SIGINT)
                tracer.wait(timeout=10)
                thread.join(timeout=10)
                for e in slot_events:
                    save(e)
                if not slot_events:
                    raise RuntimeError("no complete successful KVM slot capture")
                pids = set()
                for e in slot_events:
                    with open(f"/proc/{e['tid']}/status") as status:
                        pids.add(int(next(line.split()[1] for line in status if line.startswith("Tgid:"))))
                if len(pids) != 1:
                    raise RuntimeError("KVM slots involve multiple VMM processes")
                args.pid = pids.pop()
                slots = slot_events
            else:
                slots = json.loads(Path(args.slots).read_text())
            if args.vm_info:
                info = json.loads(Path(args.vm_info).read_text())
                args.vm_id = info["VMID"]
                connection = http.client.HTTPConnection("localhost", timeout=5)
                connection.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                connection.sock.connect(info["SocketPath"])
                connection.request("GET", "/machine-config")
                response = connection.getresponse()
                config = json.loads(response.read())
                connection.close()
                save({"event": "machine_configuration", "status": response.status, "config": config})
                if response.status != 200 or config.get("vcpu_count") != 2 or config.get("mem_size_mib") != 4096:
                    raise RuntimeError("actual Firecracker resource configuration differs from 2 vCPU/4096 MiB")
            observer = IdleObserver(args.pid, args.vm_id, slots)
            save({"event": "vmm_identity", "pid": args.pid, "start_ticks": observer.start})
            mapping = next(e for e in events if e["event"] == "mapping")
            gpas = mapping["gpas"]
            cases = [("no-touch", "n", []), ("read", "r 0 256", range(256)),
                     ("write", "w 256 256", range(256, 512)),
                     ("disjoint", "r 512 256", range(512, 768)),
                     ("recurrence", "r 0 256", range(256)),
                     ("growth", "r 0 512", range(512))]
            for cadence in gate["cadences_seconds"]:
                for repeat in range(gate["target_repetitions"]):
                    for name, command, indices in cases:
                        # Pair on/off latency for the exact same operation. The off
                        # invocation intentionally warms translations, testing recurrence.
                        off_start = time.monotonic_ns()
                        send(command)
                        off = until("done")[-1]
                        off_end = time.monotonic_ns()
                        cpu_before = time.process_time_ns()
                        observer.arm(gpas)
                        host_begin = time.monotonic_ns()
                        send(command)
                        on = until("done")[-1]
                        host_end = time.monotonic_ns()
                        time.sleep(cadence)
                        reading = observer.collect()
                        cpu_after = time.process_time_ns()
                        expected = {gpas[i] for i in indices}
                        actual = set(reading["accessed_gpas"])
                        unknown = {int(g) for g in reading["unknown_gpas"]}
                        negatives = set(gpas) - expected
                        recall = len(actual & expected) / len(expected) if expected else None
                        fp = len(actual & negatives) / len(negatives) if negatives else None
                        record = {"event": "case", "case": name, "cadence_seconds": cadence,
                                  "repeat": repeat, "reading": reading,
                                  "expected_gpas": sorted(expected),
                                  "recall_including_unknown_as_miss": recall,
                                  "false_positive_fraction": fp, "unknown_pages": len(unknown),
                                  "off_host_roundtrip_ns": off_end-off_start,
                                  "on_host_roundtrip_ns": host_end-host_begin,
                                  "off_guest_duration_ns": off["end_ns"]-off["begin_ns"],
                                  "on_guest_duration_ns": on["end_ns"]-on["begin_ns"],
                                  "observer_cpu_ns": cpu_after-cpu_before,
                                  "target_gate_pass": not unknown and (recall is None or recall >= gate["target_min_recall"]) and (fp is None or fp <= gate["target_max_false_positive_fraction"])}
                        records.append(record)
                        save(record)
            # This bounded first gate must pass before more elaborate schedules.
            passed = all(r["target_gate_pass"] for r in records)
            extended_executed = False
            full_results = []
            extended_ok = None
            if args.extended and (passed or args.diagnose_failed_target):
                extended_executed = True
                save({"event": "diagnostic_extension", "initial_target_gate_pass": passed,
                      "does_not_override_failed_gate": True})
                def extended_case(name, command, current_gpas, expected_indices, cadence, generation=0):
                    before_cpu = time.process_time_ns()
                    observer.arm(current_gpas)
                    send(command)
                    until("done")
                    time.sleep(cadence)
                    reading = observer.collect()
                    cpu_ns = time.process_time_ns() - before_cpu
                    expected = {current_gpas[i] for i in expected_indices}
                    actual = set(reading["accessed_gpas"])
                    negative = set(current_gpas) - expected
                    recall = len(actual & expected)/len(expected) if expected else None
                    fp = len(actual & negative)/len(negative) if negative else None
                    ok = not reading["unknown_gpas"] and (recall is None or recall >= gate["target_min_recall"]) and (fp is None or fp <= gate["target_max_false_positive_fraction"])
                    save({"event": "extended_case", "case": name, "cadence_seconds": cadence,
                          "content_generation": generation, "reading": reading,
                          "recall_including_unknown_as_miss": recall, "false_positive_fraction": fp,
                          "observer_cpu_ns": cpu_ns, "target_gate_pass": ok})
                    return ok
                extended_ok = True
                for cadence in gate["cadences_seconds"]:
                    for repeat in range(gate["target_repetitions"]):
                        old_gpas = gpas
                        send("reuse")
                        reuse_events = until("done")
                        gpas = next(e["gpas"] for e in reuse_events if e["event"] == "mapping")
                        save({"event": "lineage", "case": "reuse", "repeat": repeat,
                              "old_gpas": old_gpas, "new_gpas": gpas,
                              "same_guest_slots": len(set(old_gpas)&set(gpas)),
                              "note": "content generations differ even if physical slots recur"})
                        extended_ok &= extended_case("reuse", "r 0 256", gpas, range(256), cadence, repeat+1)
                        send("bg-start")
                        until("done")
                        extended_ok &= extended_case("concurrent", "n", gpas, range(256), cadence)
                        extended_ok &= extended_case("background", "n", gpas, range(256), cadence)
                        send("bg-stop")
                        until("done")
                        send("file")
                        file_events = until("file-ready")
                        file_gpas = next(e["gpas"] for e in file_events if e["event"] == "mapping")
                        save(next(e for e in file_events if e["event"] == "mapping"))
                        extended_ok &= extended_case("kernel-filecache", "file-read", file_gpas, range(256), cadence)
                        send("file-end")
                        until("done")
                save({"event": "extended_target_gate", "passed": extended_ok})
                # Retain only mapping metadata, never /proc/PID/mem or guest bytes.
                metadata_dir = Path(args.output).with_suffix(".metadata")
                metadata_dir.mkdir(mode=0o700)
                for name in ["maps", "smaps", "smaps_rollup", "status"]:
                    (metadata_dir/name).write_text(Path(f"/proc/{args.pid}/{name}").read_text())
                for cadence in gate["cadences_seconds"]:
                    memory_before_kib = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
                    wall_begin, cpu_begin = time.monotonic_ns(), time.process_time_ns()
                    inventory = observer.resident_inventory()
                    observer.arm(inventory["resident_gpas"])
                    send("r 0 256")
                    until("done")
                    time.sleep(cadence)
                    full = observer.collect()
                    end_inventory = observer.resident_inventory()
                    wall_ns, cpu_ns = time.monotonic_ns()-wall_begin, time.process_time_ns()-cpu_begin
                    initial, final = set(inventory["resident_gpas"]), set(end_inventory["resident_gpas"])
                    reasons = {}
                    for reason in full["unknown_gpas"].values():
                        reasons[reason] = reasons.get(reason, 0)+1
                    unknown_count = len(full["unknown_gpas"])+len(final-initial)
                    supported_fraction = (len(initial)-len(full["unknown_gpas"]))/len(initial|final) if initial|final else None
                    full_record = {"event": "whole_resident_sample", "cadence_seconds": cadence,
                          "reserved_bytes": sum(slot["memory_size"] for slot in slots),
                          "initial_resident_pages": len(initial), "final_resident_pages": len(final),
                          "new_resident_pages": len(final-initial), "lost_resident_pages": len(initial-final),
                          "unknown_pages_upper_at_endpoints": unknown_count,
                          "unknown_reasons": reasons, "supported_resident_fraction_at_endpoints": supported_fraction,
                          "observed_accessed_gpas": full["accessed_gpas"], "observed_idle_gpas": full["idle_gpas"],
                          "arm_begin_ns": full["arm_begin_ns"], "arm_end_ns": full["arm_end_ns"],
                          "scan_begin_ns": full["scan_begin_ns"], "scan_end_ns": full["scan_end_ns"],
                          "observer_cpu_ns": cpu_ns, "wall_ns": wall_ns,
                          "observer_maxrss_before_kib": memory_before_kib,
                          "observer_maxrss_after_kib": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss,
                          "observer_cpu_cores_actual_cycle": cpu_ns/wall_ns,
                          "observer_cpu_cores_requested_cadence": cpu_ns/(cadence*1e9),
                          "whole_sandbox_admitted": False,
                          "caveat": "endpoint resident union misses transient residency and mapping A-B-A; target causality not inferred"}
                    full_results.append(full_record)
                    save(full_record)
            remaining = ["transient-residency-coverage", "whole-observer-memory-and-latency-perturbation"]
            if not extended_executed:
                remaining += ["reuse", "concurrent", "background", "kernel-filecache", "full-resident-coverage", "whole-observer-overhead"]
            cpu_pass = all(r["observer_cpu_cores_actual_cycle"] <= gate["maximum_observer_cpu_cores"] for r in full_results) if full_results else None
            save({"event": "gate", "initial_target_gate_pass": passed,
                  "extended_cases_executed": extended_executed,
                  "extended_target_gate_pass": extended_ok,
                  "whole_observer_cpu_gate_pass": cpu_pass,
                  "whole_sandbox_admitted": False, "remaining_required": remaining,
                  "decision": "stop primary observer; unsupported fallback; no paid whole-sandbox H1/H2 admission"})
            print(json.dumps({"initial_cases": len(records), "initial_target_gate_pass": passed,
                              "whole_sandbox_admitted": False, "output": args.output}))
        except BaseException as exc:
            save({"event": "failure", "error_type": type(exc).__name__, "error": str(exc),
                  "whole_sandbox_admitted": False})
            raise
        finally:
            if observer:
                observer.close()
            if tracer and tracer.poll() is None:
                tracer.send_signal(signal.SIGINT)
                tracer.wait(timeout=10)
                thread.join(timeout=10)
                for e in slot_events:
                    save(e)
            try:
                send("quit")
                proc.wait(timeout=10)
            except (BrokenPipeError, subprocess.TimeoutExpired):
                proc.terminate()
                try:
                    proc.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
            selector.close()

if __name__ == "__main__":
    main()
