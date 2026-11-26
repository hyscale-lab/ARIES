"""Run one Crab arm on the worker. CWD contains guest, replay, fixture and qemu.pid."""
import json
import os
from pathlib import Path
import uuid
import shlex
import subprocess
import sys
import threading
import time

arm, label, profile_name = sys.argv[1:]
helpers = Path(__file__).resolve().parent
assert arm in ("checkpoint", "control")
out = Path(label)
out.mkdir()
pid = int(Path("qemu.pid").read_text())
stop = threading.Event()


def guest(*argv, source=None):
    result = subprocess.run(["./guest", shlex.join(argv)], text=True, capture_output=True, input=source)
    with (out / "commands.jsonl").open("a") as f:
        print(json.dumps(dict(argv=argv, rc=result.returncode, stderr=result.stderr)), file=f)
    if result.returncode:
        raise RuntimeError(f"{argv}: {result.stderr}")
    return result.stdout.strip()


def action(op, *args):
    return guest("sudo", "/opt/crab/venv/bin/python", "-", op, name, *args, source=(helpers / "action.py").read_text())


def mark(event, **attrs):
    with (out / "events.jsonl").open("a") as f:
        print(json.dumps(dict(event=event, ns=time.monotonic_ns(), **attrs)), file=f)


def sample():
    with (out / "host.jsonl").open("w") as f:
        while not stop.is_set():
            roll = Path(f"/proc/{pid}/smaps_rollup").read_text().splitlines()
            mem = {key: int(value.split()[0]) for row in roll if ": " in row for key, value in [row.split(": ", 1)]}
            group = Path("/sys/fs/cgroup/system.slice/zb-crab-vmm.service")
            cg = {}
            if group.exists():
                cg["current"] = int((group/"memory.current").read_text())
                cg.update({k: int(v) for k, v in (line.split() for line in (group/"memory.stat").read_text().splitlines())
                           if k in ("file", "anon", "kernel")})
            print(json.dumps(dict(cgroup=cg, ns=time.monotonic_ns(), pss_kib=mem["Pss"], file_kib=mem["Pss_File"],
                                  anon_kib=mem["Pss_Anon"], shmem_kib=mem["Pss_Shmem"])), file=f, flush=True)
            stop.wait(.1)


name = "calendar-" + uuid.uuid4().hex[:20]
initial = guest("sudo", "python3", "-", profile_name, "before", source=(helpers / "profile.py").read_text())
(out / "profile-before.json").write_text(initial + "\n")
mark("prepare")
try:
    created = action("create")
except Exception:
    try:
        action("kill")
    except RuntimeError as cleanup_error:
        print(cleanup_error, file=sys.stderr)
    raise
assert created == name
try:
    group = action("group")
    before = action("token")
    assert len(before.split(":")[1]) == 48
    sampling = threading.Thread(target=sample)
    sampling.start()
    guest_clock = guest("python3", "-c", "import time;print(time.monotonic_ns())")
    mark("guest_clock", guest_ns=int(guest_clock))
    inner_log = (out / "inner.jsonl").open("w")
    with (helpers / "inner.py").open() as source:
        inner = subprocess.Popen(["./guest", shlex.join(["sudo", "python3", "-", group])], stdin=source, stdout=inner_log)
    try:
        mark("ready", probe=before, sandbox=name, cgroup=group)
        if arm == "checkpoint":
            mark("checkpoint_request")
            ckpt = action("checkpoint")
            mark("released", checkpoint=ckpt)
        state = guest("sudo", "bash", "-c",
                      's="$1"; if [[ -f "$s/cgroup.procs" ]]; then cat "$s/cgroup.procs"; fi',
                      "bash", group)
        assert bool(state) == (arm == "control"), (arm, state)
        mark("hold_start", cgroup_pids=state.splitlines())
        time.sleep(5)
        mark("hold_end")
        if arm == "checkpoint":
            image_dir = f"/var/lib/crab/runtime/checkpoints/{name}/{ckpt}"
            allocated = guest("sudo", "du", "-sB1", image_dir)
            metadata = guest("sudo", "du", "-sB1", f"/var/lib/crab/checkpoints/artifacts/{name}",
                             f"/var/lib/crab/checkpoints/manifests/{name}")
            logical = guest("sudo", "du", "-sb", image_dir)
            snapshots = guest("sudo", "zfs", "list", "-t", "snapshot", "-p", "-o", "name,used,referenced")
            (out / "disk.json").write_text(json.dumps(dict(process_and_work_allocated=allocated, metadata_dir_allocated=metadata,
                                                           apparent=logical, zfs_snapshots=snapshots)))
        if arm == "checkpoint":
            mark("restore_request")
            action("restore", ckpt)
        after = action("token")
        mark("token_reply", probe=after)
        assert after == before and action("group") == group
        profile = guest("sudo", "python3", "-", profile_name, "token", source=(helpers / "profile.py").read_text())
        (out / "profile-token.json").write_text(profile + "\n")
        mark("replay_start")
        print(json.dumps({"sandbox": name}), flush=True)
        if sys.stdin.readline().strip() != "ok":
            raise RuntimeError("calendar replay failed")
        mark("replay_end")
        (out / "verify.json").write_text(action("verify") + "\n")
    finally:
        stop.set()
        sampling.join()
        inner.terminate()
        inner.wait()
        inner_log.close()
finally:
    action("kill")
