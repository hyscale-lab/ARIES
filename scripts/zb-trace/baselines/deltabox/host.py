"""Run a matched appliance arm and collect host and guest memory and time."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import sys
import threading
import time

root = Path(__file__).resolve().parent
trace_sha = "105845c532ae7051e61f44a6c46367e63081d2973cdb5d8d057c3abc85c44180"
kernel_sha = "0ba5e9ee744bcb2bac3391e967cf36cfeb10fd3add067055321cb136cd5004c0"


def read_plan(path):
    raw = path.read_bytes()
    assert hashlib.sha256(raw).hexdigest() == trace_sha
    batches = []
    for line in raw.splitlines():
        row = json.loads(line)
        m = row.get("message", {})
        role = m.get("role")
        if role not in ("user", "toolResult", "assistant"):
            continue
        ts = datetime.datetime.fromisoformat(row["timestamp"].replace("Z", "+00:00"))
        if role == "assistant":
            calls = [item for item in m["content"] if item["type"] == "toolCall"]
            batches.append((int((ts - last).total_seconds() * 1e6) * 1000, calls))
        else:
            last = ts
    assert [len(c) for _, c in batches] == [3, 1, 1, 0]
    assert [t for t, _ in batches] == [6068000000, 89202000000, 2873000000, 4360000000]
    return batches


def tool_request(call):
    args = call["arguments"]
    if call["name"] == "read":
        return dict(op="exec", argv=["/bin/cat", args["path"]], cwd="/root", timeout=30)
    assert call["name"] == "exec", call["name"]
    return dict(op="exec", argv=["/bin/bash", "-c", args["command"]], cwd=args["workdir"],
                timeout=args.get("timeout", 30))


def qemu_command():
    kernel = root / "ae/linux/vmlinux"
    assert hashlib.sha256(kernel.read_bytes()).hexdigest() == kernel_sha
    qemu = root / "apt/root/usr"
    cmd = [str(qemu / "bin/qemu-system-x86_64"), "-L", str(qemu / "share/qemu"),
           "-machine", "microvm,accel=kvm,acpi=off", "-cpu", "host", "-m", "512M", "-smp", "1",
           "-kernel", str(kernel), "-append",
           "console=ttyS0 loglevel=3 root=/dev/vda1 ro init=/usr/local/lib/deltabox-direct/init panic=-1 direct.mode=check",
           "-nodefaults", "-no-reboot", "-display", "none", "-monitor", "none", "-serial", "stdio", "-nic", "none"]
    for driver, name, filename in (("qcow2", "appliance", "calendar-direct.qcow2"),
                                   ("raw", "fixture", "calendar-fixture.ext4"),
                                   ("raw", "verifier", "calendar-verifier.ext4")):
        cmd += ["-blockdev", json.dumps({"driver": driver, "node-name": name, "read-only": True,
                                        "file": {"driver": "file", "filename": str(root / "images" / filename)}}),
                "-device", f"virtio-blk-device,drive={name}"]
    return cmd


class Appliance:
    def __init__(self, stream=sys.stdout):
        self.stream = stream
        self.lock = threading.Lock()
        self.reply = queue.Queue()
        self.stop = threading.Event()
        self.ident = 0
        cmd = qemu_command()
        inputs = [root / "ae/linux/vmlinux", *(root / "images" / name for name in (
            "DeltaBox-ATC2026-v3-l1.qcow2", "calendar-direct.qcow2", "calendar-fixture.ext4", "calendar-verifier.ext4"))]
        for path in inputs:
            with path.open("rb") as f:
                os.posix_fadvise(f.fileno(), 0, 0, os.POSIX_FADV_DONTNEED)
        cache = subprocess.check_output([str(root / "apt/fincore/usr/bin/fincore"), "--bytes", "--noheadings", "--output", "RES,SIZE,FILE",
                                         *map(str, inputs)], text=True)
        self.emit("file_cache_at_launch", output=cache)
        self.emit("qemu_command", argv=cmd)
        libs = root / "apt/root/usr/lib/x86_64-linux-gnu"
        self.proc = subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                     stderr=subprocess.STDOUT, env=dict(os.environ, LD_LIBRARY_PATH=str(libs)))
        self.emit("qemu_started", pid=self.proc.pid)
        self.reader = threading.Thread(target=self.read, daemon=True)
        self.sampler = threading.Thread(target=self.sample, daemon=True)
        self.reader.start()
        self.sampler.start()

    def emit(self, event, **data):
        with self.lock:
            now = time.monotonic_ns()
            print(json.dumps(dict(event=event, host_monotonic_ns=now, **data)), file=self.stream, flush=True)
            return now

    def memory(self, phase):
        proc = Path(f"/proc/{self.proc.pid}")
        mem = {}
        for line in (proc / "smaps_rollup").read_text().splitlines()[1:]:
            key, value = line.split(":", 1)
            if value:
                mem[key] = int(value.split()[0])
        stat = (proc / "stat").read_text().rsplit(")", 1)[1].split()
        return self.emit("host_sample", phase=phase, pid=self.proc.pid, pss_kib=mem["Pss"],
                         pss_anon_kib=mem["Pss_Anon"], pss_file_kib=mem["Pss_File"], pss_shmem_kib=mem["Pss_Shmem"],
                         cpu_ticks=int(stat[11]) + int(stat[12]))

    def sample(self):
        while not self.stop.wait(.25):
            try:
                self.memory("periodic")
            except (OSError, ProcessLookupError):
                return

    def read(self):
        try:
            for raw in self.proc.stdout:
                line = raw.decode(errors="replace").strip()
                if "ZB_EVENT " in line:
                    try:
                        row, _ = json.JSONDecoder().raw_decode(line.split("ZB_EVENT ", 1)[1])
                    except json.JSONDecodeError:
                        self.emit("serial_parse_error", line=line)
                        continue
                    now = self.emit("guest", payload=row)
                    if row["event"] in ("ready", "reply", "controller_failure"):
                        self.reply.put((now, row))
                else:
                    self.emit("serial", line=line)
        finally:
            self.reply.put((time.monotonic_ns(), {"error": "QEMU serial closed"}))

    def receive(self, timeout):
        try:
            when, row = self.reply.get(timeout=timeout)
        except queue.Empty:
            raise TimeoutError("guest serial reply timeout") from None
        if "error" in row:
            raise RuntimeError(row["error"])
        return when, row

    def request(self, operation, **kwargs):
        self.ident += 1
        when = self.emit("request", operation=operation, id=self.ident)
        self.proc.stdin.write((json.dumps(dict(id=self.ident, op=operation, **kwargs)) + "\n").encode())
        self.proc.stdin.flush()
        reply_at, row = self.receive(90)
        assert row["id"] == self.ident, row
        return when, reply_at, row["response"]

    def close(self):
        self.stop.set()
        if self.proc.poll() is None:
            self.proc.terminate()
        try:
            code = self.proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            code = self.proc.wait()
        self.reader.join()
        self.sampler.join()
        self.emit("qemu_stopped", exit_code=code)


def run(args):
    plan = read_plan(args.trace)
    expected_hashes = args.hashes.read_text().strip()
    appliance = Appliance()
    op = "startup"
    try:
        _, ready = appliance.receive(120)
        assert ready["event"] == "ready" and ready["guest_cpu_count"] == 1, ready
        assert 400 * 1024 < int(ready["guest_memtotal"].split()[1]) <= 512 * 1024, ready
        op = "prepare token"
        _, _, probe = appliance.request("token")
        assert len(probe["token"]) == 32
        appliance.memory("pre_capture")
        arm_start = appliance.emit("arm_start", arm=args.arm)
        if args.arm == "checkpoint":
            op = "checkpoint and stop worker"
            checkpoint_start, release, receipt = appliance.request("checkpoint")
            assert receipt["worker_stopped_pid"] == probe["pid"], receipt
        else:
            release = arm_start
            checkpoint_start = None
        hold_begin = appliance.memory("hold_start")
        time.sleep(5)
        hold_end = appliance.memory("hold_end")
        op = "restore and read token" if args.arm == "checkpoint" else "read control token"
        restore_start, token_read, restored = appliance.request("restore" if args.arm == "checkpoint" else "token")
        assert restored["token"] == probe["token"] and restored["inner_pid"] == probe["inner_pid"], restored
        if args.arm == "checkpoint":
            assert restored["workspace_rollback"] and restored["profile"] == ready["profile"]
            assert restored["restore_path"] == "warm-template" and restored["pid"] != probe["pid"], restored
        else:
            assert restored["pid"] == probe["pid"], restored
        appliance.memory("post_restore")
        op = "calendar replay"
        exits = []
        for index, (wait_ns, calls) in enumerate(plan):
            wait_start = appliance.emit("source_wait", batch=index, requested_ns=wait_ns)
            time.sleep(wait_ns / 1e9)
            appliance.emit("source_wait_end", batch=index, actual_ns=time.monotonic_ns() - wait_start)
            if not calls:
                continue
            started, finished, reply = appliance.request("replay", calls=[tool_request(call) for call in calls])
            if index == 0:
                first_start, first_end = started, finished
            assert len(reply["results"]) == len(calls)
            for call, result in zip(calls, reply["results"]):
                assert "error" not in result, result
                exits.append(result["exit"])
                appliance.emit("tool_reply", call_id=call["id"], host_dispatch_ns=started,
                               host_reply_ns=finished, **result)
        end = appliance.emit("replay_end")
        assert exits == [0, 0, 0, 128, 0], exits
        op = "official verifier and artifact hashes"
        _, _, verify = appliance.request("verify")
        assert verify["reward"]["overall_score"] == 1.0 and verify["hashes"].strip() == expected_hashes, verify
        appliance.emit("verified", arm=args.arm, arm_start_ns=arm_start, checkpoint_start_ns=checkpoint_start,
                       release_ns=release, hold_start_ns=hold_begin, hold_end_ns=hold_end, restore_ns=restore_start,
                       token_read_ns=token_read, first_batch_start_ns=first_start, first_batch_end_ns=first_end,
                       replay_end_ns=end, **verify)
        op = "shutdown"
        appliance.request("shutdown")
    except Exception as exc:
        appliance.emit("first_failure", operation=op, error=repr(exc))
        raise
    finally:
        appliance.close()


if __name__ == "__main__":
    def interrupt(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupt)
    signal.signal(signal.SIGHUP, interrupt)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("arm", choices=("checkpoint", "control"))
    parser.add_argument("trace", type=Path)
    parser.add_argument("hashes", type=Path)
    run(parser.parse_args())
