#!/usr/bin/env python3
"""Hash the deployed Firecracker stack while the study worker has no active VM."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time
import tomllib


def run(*command):
    return subprocess.check_output(command, text=True).strip()


def idle(ctr, sockets):
    tasks = {str(sock): run(ctr, "--address", sock, "--namespace", "aries", "tasks", "list", "-q").splitlines()
             for sock in sockets}
    vmms = {}
    for process in Path("/proc").iterdir():
        if not process.name.isdecimal():
            continue
        try:
            filename = os.readlink(process / "exe")
        except FileNotFoundError:
            continue
        name = Path(filename.removesuffix(" (deleted)")).name
        if name == "firecracker" or name.startswith(".firecracker-real"):
            vmms[process.name] = filename
    if any(tasks.values()) or vmms:
        raise RuntimeError(f"active guest discovered: {tasks} {vmms}")
    return {"tasks_by_socket": tasks, "vmm_processes": vmms}


def hash_file(path):
    with path.open("rb") as stream:
        before = os.fstat(stream.fileno())
        digest = hashlib.file_digest(stream, "sha256").hexdigest()
        after = os.fstat(stream.fileno())
    if (before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_ino, after.st_size, after.st_mtime_ns):
        raise RuntimeError(f"file changed during readback: {path}")
    return {"path": str(path), "bytes": after.st_size, "sha256": digest}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("unit")
    p.add_argument("runtime", type=Path)
    p.add_argument("--also-socket", action="append", type=Path, default=[])
    a = p.parse_args()
    runtime = a.runtime.resolve(strict=True)
    daemon = runtime / "bin/firecracker-containerd"
    ctr = runtime / "bin/firecracker-ctr"
    pid = int(run("systemctl", "show", "--property=MainPID", "--value", a.unit))
    process = Path("/proc") / str(pid)
    if not process.joinpath("exe").samefile(daemon):
        raise RuntimeError("running service differs from the selected containerd executable")
    args = process.joinpath("cmdline").read_bytes().rstrip(b"\x00").split(b"\x00")
    startup_path = Path(os.fsdecode(args[args.index(b"--config") + 1]))
    if startup_path.resolve(strict=True) != runtime / "containerd.toml":
        raise RuntimeError("service did not start with the selected configuration")
    env = dict(item.split(b"=", 1) for item in process.joinpath("environ").read_bytes().split(b"\x00") if b"=" in item)
    config_path = Path(os.fsdecode(env[b"FIRECRACKER_CONTAINERD_RUNTIME_CONFIG_PATH"]))
    if config_path.resolve(strict=True) != runtime / "runtime.json":
        raise RuntimeError("guest runtime configuration differs")
    cfg = json.loads(config_path.read_bytes())
    host_cfg = tomllib.loads(startup_path.read_text())
    socket = Path(host_cfg["plugins"]["io.containerd.server.v1.grpc"]["address"])
    sockets = [socket, *a.also_socket]
    check_before = idle(ctr, sockets)
    start = time.monotonic_ns()
    paths = {"firecracker": Path(cfg["firecracker_binary_path"]), "kernel": Path(cfg["kernel_image_path"]),
             "rootfs": Path(cfg["root_drive"]), "daemon": daemon, "live_daemon": process / "exe",
             "shim": runtime / "bin/containerd-shim-aws-firecracker", "ctr": ctr, "runc": Path(cfg["jailer"]["runc_binary_path"]),
             "jailer": runtime / "bin/jailer", "jailer_config": Path(cfg["jailer"]["runc_config_path"]),
             "runtime_config": config_path, "containerd_config": startup_path}
    files = {name: hash_file(path) for name, path in paths.items()}
    if files["daemon"]["sha256"] != files["live_daemon"]["sha256"]:
        raise RuntimeError("service executable and deployed file differ")
    versions = {"firecracker": run(paths["firecracker"], "--version"), "containerd": run(daemon, "--version"),
                "ctr_server": run(ctr, "--address", socket, "version")}
    check_after = idle(ctr, sockets)
    if int(run("systemctl", "show", "--property=MainPID", "--value", a.unit)) != pid:
        raise RuntimeError("service process changed during verification")
    print(json.dumps({"schema": "aries.harbor.idle-runtime-readback.v1",
                      "node": Path("/var/emulab/boot/nodeid").read_text().strip(), "unit": a.unit, "daemon_pid": pid,
                      "utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                      "begin_monotonic_ns": start, "end_monotonic_ns": time.monotonic_ns(),
                      "config": {k: cfg[k] for k in ("guest_root_mode", "kernel_args", "smt_enabled")},
                      "before": check_before, "after": check_after, "versions": versions, "files": files}, indent=2))


if __name__ == "__main__":
    main()
