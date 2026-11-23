"""Record worker-wide stock and restored Firecracker lifetimes around one replay."""
import argparse
import json
from pathlib import Path
import subprocess
import time


VMM_NAMES = {"firecracker", ".firecracker-re"}


def processes(proc=Path("/proc")):
    rows = []
    for path in proc.glob("[0-9]*/stat"):
        try:
            value = path.read_text()
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
        end = value.rindex(")")
        name = value[value.index("(") + 1:end]
        if name in VMM_NAMES:
            start_ticks = int(value[end + 2:].split()[19])
            rows.append({"pid": int(path.parent.name), "comm": name, "start_ticks": start_ticks})
    return sorted(rows, key=lambda row: row["pid"])


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("output", type=Path)
    cli.add_argument("command", nargs=argparse.REMAINDER)
    args = cli.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command:
        cli.error("supply the replay command after the output path")

    boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    with args.output.open("x", buffering=1) as log:
        def sample(phase):
            before = time.monotonic_ns()
            active = processes()
            after = time.monotonic_ns()
            log.write(json.dumps({"phase": phase, "boot_id": boot, "begin_ns": before,
                                  "end_ns": after, "realtime_ns": time.time_ns(), "vmms": active}) + "\n")
            return active

        if sample("before_replay"):
            raise SystemExit("worker already has a Firecracker process")
        with subprocess.Popen(command) as replay:
            exclusive = True
            while True:
                try:
                    rc = replay.wait(timeout=1)
                    break
                except subprocess.TimeoutExpired:
                    exclusive &= len(sample("during_replay")) <= 1
        idle = not sample("after_replay")
    if rc:
        raise SystemExit(rc)
    if not exclusive or not idle:
        raise SystemExit("worker-wide sampling detected a concurrent or surviving Firecracker process")


if __name__ == "__main__":
    main()
