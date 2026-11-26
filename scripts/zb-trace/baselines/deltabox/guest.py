"""Appliance controller. Runtime checkpoint/restore and DeltaFS remain stock."""
import asyncio
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import tty
from pathlib import Path

fixture = Path("/mnt/fixture")
runtime = fixture / "runtime"
sys.path.insert(0, str(runtime))
from agent.sandbox_driver import SandboxDriver
from backends.deltabox.local_backend import LocalDeltaBoxBackend
workspace_path = "/tmp_workspace"
artifacts = ["decision_log.md", "scheduled.ics", "unscheduled.json"]
lock = threading.Lock()
serial_output = os.fdopen(os.dup(sys.stdout.fileno()), "w", buffering=1)


def emit(event, **fields):
    with lock:
        print("ZB_EVENT " + json.dumps(dict(event=event, guest_monotonic_ns=time.monotonic_ns(), **fields)),
              file=serial_output, flush=True)


def inner_sample(stop, state):
    while not stop.is_set():
        processes = []
        for proc in Path("/proc").iterdir():
            if not proc.name.isnumeric():
                continue
            try:
                stat = (proc / "stat").read_text()
                tail = stat.rsplit(")", 1)[1].split()
                pss = next(int(line.split()[1]) for line in (proc / "smaps_rollup").read_text().splitlines()
                           if line.startswith("Pss:"))
            except (OSError, StopIteration, IndexError, ValueError):
                continue
            processes.append({"pid": int(proc.name), "comm": stat.split("(", 1)[1].rsplit(")", 1)[0],
                              "state": tail[0], "cpu_ticks": int(tail[11]) + int(tail[12]), "pss_kib": pss})
        counters = {line.split(":", 1)[0]: int(line.split()[1]) for line in Path("/proc/meminfo").read_text().splitlines()
                    if line.split(":", 1)[0] in {"MemAvailable", "Cached", "Shmem"}}
        emit("inner_sample", **state, memory_kib=counters, processes=processes)
        stop.wait(.25)


def profile():
    damon = Path("/sys/module/damon_reclaim/parameters")
    virtio = Path("/sys/bus/virtio/devices")
    balloon = [p.name for p in virtio.iterdir() if (p / "device").read_text().strip() == "0x0005"]
    order = Path("/sys/module/page_reporting/parameters/page_reporting_order")
    return {"damon_parameters": damon.is_dir(), "balloon_devices": balloon,
            "reporting_order": order.read_text().strip() if order.exists() else None,
            "kernel": os.uname().release, "swaps": Path("/proc/swaps").read_text().splitlines()[1:]}


def storage_size(root):
    return {"logical_bytes": sum(p.stat().st_size for p in root.rglob("*") if p.is_file()),
            "allocated_bytes": sum(p.stat().st_blocks * 512 for p in root.rglob("*") if p.is_file())}



def run_controller(driver, backend, runner):
    work = driver.workdir
    workspace = driver.merged_dir
    checkpoint = None
    closed = False
    scored = False
    state = {"worker_pid": None, "template_pid": None}
    stop = threading.Event()
    sampler = threading.Thread(target=inner_sample, args=(stop, state), daemon=True)

    def python(code, timeout=30):
        result = runner.run_until_complete(backend.exec_action({"type": "python", "code": code, "timeout_s": timeout}))
        assert result["rc"] == 0 and not result["error"], result
        return result["stdout"]

    def token():
        observed = json.loads(python("print(json.dumps(dict(token=calendar_token, inner_pid=os.getpid())))"))
        return dict(observed, pid=driver._ctrl.agent_pid)

    try:
        caps = profile()
        assert not caps["damon_parameters"] and not caps["balloon_devices"] and not caps["swaps"], caps
        shutil.copyfile(fixture / "USER.md", "/root/USER.md")
        runner.run_until_complete(backend.setup())
        env = dict(os.environ, HOME="/root", GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_NOSYSTEM="1", LC_ALL="C.UTF-8")
        subprocess.run(["git", "-C", workspace, "init", "-q", "--initial-branch=master"], env=env, check=True)
        marker = workspace / ".git/info/zb-checkpoint-state"
        marker.write_text("saved\n")
        controller = driver._ctrl
        assert controller.enable_warm_template and controller.fixed_active_pid == 100
        python("import json, os, secrets; calendar_token = secrets.token_hex(16)")
        state["worker_pid"] = controller.agent_pid
        versions = {
            "sandbox_controller_sha256": hashlib.sha256((runtime / "backends/deltabox/gsd/sandbox_controller.py").read_bytes()).hexdigest(),
            "criu_dump_bin": controller.criu_dump_bin,
            "criu": subprocess.check_output([controller.criu_dump_bin, "--version"], text=True).strip(),
            "python": sys.version.split()[0],
        }
        sampler.start()
        with open("/dev/ttyS0", "r", encoding="utf-8") as serial:
            tty.setraw(serial.fileno())
            emit("ready", profile=caps, versions=versions, checkpoint_profile="runtime-default",
                 guest_cpu_count=os.cpu_count(), guest_memtotal=Path("/proc/meminfo").read_text().splitlines()[0])
            for line in serial:
                request = json.loads(line)
                ident, op = request["id"], request["op"]
                try:
                    if op == "token":
                        response = token()
                    elif op == "checkpoint":
                        checkpoint = runner.run_until_complete(backend.checkpoint("calendar"))
                        meta = checkpoint.handle.meta
                        template_pid = controller.template_pool.get(checkpoint.handle.ckpt_id)
                        assert template_pid and template_pid == meta["template_pid"], repr(meta)
                        assert template_pid != controller.agent_pid
                        marker.write_text("changed\n")
                        (workspace / ".git/info/zb-child-only").touch()
                        active = controller.agent_pid
                        # Use the controller's own release operation, including its async-dump guard.
                        controller._kill_active_subtree()
                        assert not Path(f"/proc/{active}").exists(), f"active worker remained: {active}"
                        assert Path(f"/proc/{template_pid}/stat").read_text().rsplit(")", 1)[1].split()[0] == "T"
                        state["worker_pid"], state["template_pid"] = None, template_pid
                        response = {"worker_stopped_pid": active, "template_pid": template_pid,
                                    "mode": "DeltaCR warm-template plus DeltaFS",
                                    "criu_tmpfs": storage_size(Path(meta["mem_path"])),
                                    "frozen_layer_tmpfs": storage_size(Path(meta["layers"][0])),
                                    "checkpoint_disk_bytes": 0}
                    elif op == "restore":
                        backend._close_io()
                        assert checkpoint is not None
                        runner.run_until_complete(backend.restore(checkpoint))
                        restore_meta = backend.last_restore_rec.meta
                        assert restore_meta["path"] == "warm-template", restore_meta
                        state["worker_pid"] = controller.agent_pid
                        assert marker.read_text() == "saved\n" and not (workspace / ".git/info/zb-child-only").exists()
                        response = dict(token(), workspace_rollback=True, profile=profile(),
                                        restore_path=restore_meta["path"], template_pid=state["template_pid"])
                    elif op == "replay":
                        assert not scored, "tool calls are disabled during verification"
                        calls = work / "calendar-calls.json"
                        calls.write_text(json.dumps(request["calls"]))
                        response = {"results": json.loads(python(
                            f"exec(open('/mnt/fixture/worker.py').read()); run_batch({str(calls)!r})",
                            timeout=max(r["timeout"] for r in request["calls"]) + 5))}
                    elif op == "verify":
                        scored = True
                        backend._close_io()
                        controller._kill_active_subtree()
                        assert not Path(f"/proc/{controller.agent_pid}").exists(), "worker remained at verifier boundary"
                        state["worker_pid"] = None
                        Path("/tests").mkdir(exist_ok=True)
                        subprocess.run(["mount", "-t", "ext4", "-o", "ro", "/dev/vdc", "/tests"], check=True)
                        verify = subprocess.run(["/bin/bash", "/tests/test.sh"], cwd=workspace,
                                                capture_output=True, text=True, check=True)
                        hashes = subprocess.check_output(["sha256sum", *[f"results/{name}" for name in artifacts]],
                                                        cwd=workspace, text=True)
                        response = {"verifier": {"exit": verify.returncode, "stdout": verify.stdout, "stderr": verify.stderr},
                                    "hashes": hashes, "reward": json.loads(Path("/logs/verifier/reward.json").read_text())}
                    elif op == "shutdown":
                        try:
                            runner.run_until_complete(backend.teardown())
                        finally:
                            closed = True
                        emit("reply", id=ident, response={})
                        return
                    else:
                        raise ValueError(f"unknown operation {op!r}")
                    emit("reply", id=ident, response=response)
                except Exception as exc:
                    emit("reply", id=ident, error=repr(exc))
                    raise
    finally:
        stop.set()
        if sampler.ident is not None:
            sampler.join()
        try:
            if not closed:
                runner.run_until_complete(backend.teardown())
        finally:
            for logfile in work.rglob("*.log"):
                lines = logfile.read_text(errors="replace").splitlines()
                if lines:
                    emit("runtime_log", path=str(logfile.relative_to(work)), tail=lines[-60:])


def main():
    state = Path(tempfile.mkdtemp(prefix="zb-deltacr-", dir="/tmp"))
    subprocess.run(["mount", "-t", "tmpfs", "-o", "noswap", "tmpfs", state], check=True)
    driver = SandboxDriver(state, fixture / "environment", sudo_wrap=False)
    driver.merged_dir = Path(workspace_path)
    backend = LocalDeltaBoxBackend(driver)
    runner = asyncio.new_event_loop()
    with (state / "controller.log").open("w", buffering=1) as log:
        os.dup2(log.fileno(), sys.stdout.fileno())
        os.dup2(log.fileno(), sys.stderr.fileno())
    try:
        run_controller(driver, backend, runner)
    except Exception as exc:
        emit("controller_failure", error=repr(exc))
        raise
    finally:
        runner.run_until_complete(runner.shutdown_default_executor())
        runner.close()


if __name__ == "__main__":
    main()
