#!/usr/bin/env python3
"""Exercise the barrier on four disposable VMMs. Args: stock Firecracker, kernel, NEW output directory."""
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
import http.client
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import time


gate = Path(__file__).with_name("cow-gate.py").resolve()


def api(path, method, endpoint, obj=None):
    conn = http.client.HTTPConnection("localhost", timeout=15)
    conn.sock = socket.socket(socket.AF_UNIX)
    conn.sock.settimeout(15)
    try:
        conn.sock.connect(str(path))
        conn.request(method, endpoint, None if obj is None else json.dumps(obj), {"Content-Type": "application/json"})
        reply = conn.getresponse()
        data = reply.read()
        if reply.status not in (200, 204):
            raise RuntimeError(f"{method} {endpoint}: {reply.status} {data!r}")
        return json.loads(data) if data else None
    finally:
        conn.close()


def until(check):
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(.025)
    raise TimeoutError(f"waiting for {check}")


def main(binary, kernel, output):
    base = Path(output).resolve()
    base.mkdir(mode=0o700)
    ctl = base / "ctl"
    receipt = base / "receipts.jsonl"
    env = dict(os.environ, COW_GATE_FIRECRACKER=str(Path(binary).resolve()), COW_GATE_SOCKET=str(ctl))
    snap = {"snapshot_path": str(base / "state"), "mem_file_path": str(base / "mem")}

    with ExitStack() as stack:
        def spawn(args, name):
            log = stack.enter_context((base / f"{name}.log").open("wb"))
            process = subprocess.Popen(args, env=env, stdout=log, stderr=log)
            def stop():
                if process.poll() is None:
                    process.kill()
                process.wait()
            stack.callback(stop)
            return process

        spawn([gate, "serve", ctl, receipt], "gate")
        until(ctl.exists)

        def vmm(index):
            work = base / str(index)
            work.mkdir()
            front = work / "fc.sock"
            pid = spawn([gate, "--api-sock", front], str(index)).pid
            until(front.exists)
            return front, pid

        front, _ = vmm(0)
        api(front, "PUT", "/machine-config", {"vcpu_count": 1, "mem_size_mib": 128, "track_dirty_pages": True})
        api(front, "PUT", "/boot-source", {"kernel_image_path": str(Path(kernel).resolve()),
                                           "boot_args": "root=/dev/vda rootwait console=ttyS0"})
        api(front, "PUT", "/actions", {"action_type": "InstanceStart"})
        api(front, "PATCH", "/vm", {"state": "Paused"})
        api(front, "PUT", "/snapshot/create", snap)
        dirty = api(front, "GET", "/vm/dirty-memory-ranges")
        assert dirty == json.loads(next(base.glob("dirty-*.json")).read_bytes())

        children = [vmm(i + 1) for i in range(4)]
        params = {"snapshot_path": snap["snapshot_path"], "mem_backend": {"backend_type": "File",
                  "backend_path": snap["mem_file_path"]}, "resume_vm": False, "track_dirty_pages": True}
        def states():
            return json.loads(subprocess.check_output([gate, "list", ctl]))
        with ThreadPoolExecutor(4) as workers:
            def restore(child):
                sock, _ = child
                api(sock, "PUT", "/snapshot/load", params)
                api(sock, "PATCH", "/vm", {"state": "Resumed"})
            futures = [workers.submit(restore, child) for child in children]
            def held():
                for future in futures:
                    if future.done():
                        future.result()
                        raise AssertionError("resume completed before release")
                vms = [vm for vm in states() if vm["state"] == "held"]
                return vms if len(vms) == 4 else None
            vms = until(held)
            try:
                api(vms[0]["socket"], "PATCH", "/vm", {"state": "Resumed"})
            except RuntimeError as exc:
                assert "another VM mutation is pending" in str(exc)
            else:
                raise AssertionError("duplicate resume bypassed the hold")
            assert all(api(vm["real_socket"], "GET", "/")["state"] == "Paused" for vm in vms)
            subprocess.run([gate, "release", ctl, *(str(pid) for _, pid in children)], check=True)
            for future in futures:
                future.result()
            assert all(api(vm["real_socket"], "GET", "/")["state"] == "Running" for vm in vms)
        vms = [vm for vm in states() if vm["state"] == "resumed"]
        assert len(vms) == 4
        for vm in vms:
            assert vm["load_done_ns"] <= vm["held_ns"] <= vm["resume_send_ns"] <= vm["resume_sent_ns"] <= vm["resume_reply_ns"]
        starts = [vm["resume_send_ns"] for vm in vms]
        replies = [vm["resume_reply_ns"] for vm in vms]
        print(json.dumps(dict(verified=len(vms), send_skew_ns=max(starts)-min(starts),
                              reply_skew_ns=max(replies)-min(replies), receipt=str(receipt))))


if __name__ == "__main__":
    main(*sys.argv[1:])
