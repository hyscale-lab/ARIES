"""Verify a closed worker lane and remove its published duplicate launch wrappers."""
import argparse
import csv
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import runpy
import shlex
import stat
import subprocess


STAGE = Path("/local/zb-bn-fit-modify-01")
RUNTIMES = {
    "amd182": "/local/zb-validation/runtime",
    "amd191": "/local/zb-fc-v4/v3/runtime",
    "amd198": "/local/zb-imagecircuit-v4/runtime",
    "amd200": "/local/zb-v2/runtime",
    "amd201": "/local/zb-cancel-async-01/deep-v1/runtime",
}
PATHS = ("queue.sh", "run-worker.sh", "worker.py")
REPLAY = "scripts/harbor-replay/replacements/bn-fit-modify"
RESULT = "plan/result/1_opportunity/replacements/bn-fit-modify"
CANONICAL = "cfa36af"


def run(*args, **opts):
    return subprocess.check_output(args, **opts)


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def published(project, raw, node):
    git = ("git", "-C", str(project))
    aries = ("git", "-C", str(project / "repo/ARIES"))
    main = run(*git, "ls-remote", "origin", "refs/heads/main", text=True).split()[0]
    pub = run(*aries, "ls-remote", "origin", "refs/heads/jy/zb-replay-trace", text=True).split()[0]
    revision = run(*aries, "rev-parse", CANONICAL, text=True).strip()
    subprocess.run([*aries, "merge-base", "--is-ancestor", revision, pub], check=True)
    workers = json.loads(run(*git, "show", f"{main}:{RESULT}/dispatch.json"))["workers"]
    placed, = [w for w in workers if w["node"] == node]
    source = {name: hashlib.sha256(run(*aries, "show", f"{revision}:{REPLAY}/{name}")).hexdigest() for name in PATHS}
    rows = csv.DictReader(run(*git, "show", f"{main}:{RESULT}/cells.csv", text=True).splitlines())
    archives = {}
    for row in rows:
        if row["node"] != node:
            continue
        receipt = json.loads(run(*git, "show", f"{main}:{RESULT}/{row['receipt']}"))
        archive = raw / receipt["raw_archive"]
        assert receipt["node"] == node and sha(archive) == receipt["worker_archive_sha256"]
        archives[receipt["run_id"]] = receipt["worker_archive_sha256"]
    expected = 2 if node == "amd201" else 5
    unit = f"zb-bn-fit-runner-{node}.service"
    control = run("systemctl", "show", unit, "-p", "ActiveState", "-p", "Result", text=True).splitlines()
    assert len(archives) == expected and {"ActiveState=inactive", "Result=success"} <= set(control)
    return dict(zepbound_main=main, aries_published_tip=pub, aries_source_revision=revision,
                shell_sha256=source, archives=archives, inventory=placed["worker_inventory_sha256"], runner_state=control)


def stage_users():
    for proc in Path("/proc").iterdir():
        if proc.name.isdecimal():
            try:
                args = (proc / "cmdline").read_bytes()
            except (FileNotFoundError, ProcessLookupError):
                continue
            assert os.fsencode(STAGE) not in args, "running process still references the BN stage"


def retire(node, proof, remove):
    assert node == Path("/var/emulab/boot/nodeid").read_text().strip() and STAGE.resolve() == STAGE
    assert (STAGE / "controller.complete").is_file()
    rows = [line.split("\t") for line in (STAGE / "queue.tsv").read_text().splitlines()]
    assert set(proof["archives"]) == {k for k, _ in rows} and len(rows) == len(proof["archives"])
    assert all((STAGE / "runs" / key / "archived").is_file() for key, _ in rows)
    state = run("systemctl", "show", "zb-bn-fit-replay-01.service", "-p", "ActiveState", "-p", "Result", text=True).splitlines()
    assert {"ActiveState=inactive", "Result=success"} <= set(state)
    stage_users()
    seal = STAGE / "SHA256SUMS"
    assert sha(seal) == proof["inventory"]
    indexed = {name: digest for digest, name in map(str.split, seal.read_text().splitlines())}
    inputs = {key: value for key, value in indexed.items() if key.startswith("sources/")}
    assert len(inputs) == 10 and all(sha(STAGE / k) == h for k, h in inputs.items())
    idle = runpy.run_path(str(STAGE / "audit-idle-runtime.py"))["idle"]
    socket_paths = sorted(Path("/run").glob("fcmod-zepbound-*/containerd.sock"))
    assert socket_paths
    ctr = Path(RUNTIMES[node]) / "bin/firecracker-ctr"
    before = idle(ctr, socket_paths)
    copies = [STAGE / name for name in PATHS]
    for copy in copies:
        file = copy.lstat()
        assert stat.S_ISREG(file.st_mode) and file.st_nlink == 1
        assert sha(copy) == indexed[copy.name] == proof["shell_sha256"][copy.name]
    if remove:
        systemd_units = run("systemctl", "list-units", "--type=service", "--state=activating,active", "--no-legend", "--plain", text=True)
        assert not any(line.startswith("zb-") and "-dns-" not in line.split()[0] for line in systemd_units.splitlines())
        stage_users()
        assert idle(ctr, socket_paths) == before
        for copy in copies:
            copy.unlink()
            assert not copy.exists()
    assert all(sha(STAGE / name) == digest for name, digest in inputs.items())
    return dict(node=node, utc=datetime.now(timezone.utc).isoformat(), stage=str(STAGE), worker_state=state, queue=rows,
                before=before, after=idle(ctr, socket_paths), proof=proof, retained_inputs=inputs,
                removed_script_paths=[str(copy) for copy in copies] if remove else [])


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("node", choices=RUNTIMES)
    parser.add_argument("raw", type=Path)
    parser.add_argument("--remove", action="store_true")
    parser.add_argument("--worker-proof", type=json.loads)
    args = parser.parse_args()
    if args.worker_proof:
        print(json.dumps(retire(args.node, args.worker_proof, args.remove), indent=2))
    else:
        project = Path(__file__).resolve().parents[6]
        proof = published(project, args.raw, args.node)
        command = ["sudo", "-n", "python3", "-B", "-", args.node, ".", "--worker-proof", json.dumps(proof)]
        if args.remove:
            command.append("--remove")
        print(run("ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", f"nehalem@{args.node}.utah.cloudlab.us",
                  shlex.join(command), input=Path(__file__).read_bytes()).decode(), end="")
