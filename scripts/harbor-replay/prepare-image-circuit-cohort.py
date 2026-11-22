#!/usr/bin/env python3
"""Package source-matched image, circuit, token-count or video recordings with validated native runners."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

from cohort_image_circuit import IMAGE_HELPER, TASKS, boundaries, digest, verify


def install(src, dst):
    dst.parent.mkdir(parents=True, exist_ok=True)
    # Keep the archived bytes independent of later edits and new native builds.
    shutil.copy2(src, dst)


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("cache", type=Path)
    cli.add_argument("receipts", type=Path)
    cli.add_argument("output", type=Path)
    cli.add_argument("--exclude-manifest", type=Path, help="omit earlier published trial IDs")
    task_group = cli.add_mutually_exclusive_group()
    task_group.add_argument("--dataset", action="store_true", help="package validated token-count recordings")
    task_group.add_argument("--video", action="store_true", help="package strict video recordings with byte-identical guest frames")
    a = cli.parse_args()
    selected = {"count-dataset-tokens"} if a.dataset else TASKS
    if a.video:
        selected = {"video-processing"}
    prior = {r["trial_id"] for r in json.loads(a.exclude_manifest.read_bytes())} if a.exclude_manifest else set()
    repo = Path(__file__).resolve().parents[2]
    replay = repo / "scripts/harbor-replay"
    bound = json.loads((repo / "pkg/replay/harbor/reviewed.json").read_bytes())
    output = a.output
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise FileExistsError(f"bundle path must be empty: {output}")
    candidates = {digest(p): p for p in a.cache.glob("aries-*") if p.is_file()}
    tracer_files = ["idle_hook.py", "idle.py", "guest_sample.py", "snapshot_probe.py",
                    "sample_vmm.py", "kvm-slots.bt", "vmm_census.py"]
    for name in tracer_files:
        install(repo / "scripts/zb-trace" / name, output / "tracer" / name)
    install(replay / "run-traced.sh", output / "tracer/run-traced.sh")
    for name in ["run-reviewed-cohort.py", "cohort_image_circuit.py"]:
        install(replay / name, output / name)
    helpers = [("codex-harbor-tools.tar.gz", "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab")]
    validate = verify
    if a.dataset or a.video:
        from cohort_dataset import CNI_SHA
        if a.dataset:
            from cohort_dataset import verify as validate
        else:
            from cohort_video import verify as validate
            install(replay / "cohort_video.py", output / "cohort_video.py")
        for name in ("cohort_dataset.py", "provision-pypi-cni.sh"):
            install(replay / name, output / name)
        helpers.append(("cni/SHA256SUMS", CNI_SHA))
        for line in (a.cache / "cni/SHA256SUMS").read_text().splitlines():
            sha, plugin = line.split()
            helpers.append(("cni/bin/" + plugin, sha))
    if not a.dataset:
        helpers.append(("aries-harbor-image", IMAGE_HELPER))
    for name, expected in helpers:
        if digest(a.cache / name) != expected:
            raise ValueError(f"incorrect source helper {name}")
        install(a.cache / name, output / name)
    entries = []
    pending = []
    for spec in bound:
        task = spec["task"]
        if task not in selected:
            continue
        for item in spec["trials"]:
            trial = item["trial"]
            if trial in prior:
                continue
            suffix = "-guest-codec" if task == "code-from-image" else ""
            reference = a.receipts / f"{task}-{trial}{suffix}.json"
            if not reference.exists():
                pending.append({"task": task, "trial_id": trial, "reason": "unavailable matching native fidelity receipt"})
                continue
            receipt = json.loads(reference.read_bytes())
            if receipt.get("diagnostic_video_images") or receipt.get("diagnostic_process_drift"):
                pending.append({"task": task, "trial_id": trial, "reason": "diagnostic receipt only"})
                continue
            if not (receipt["task"] == task and receipt["trial_id"] == trial and receipt["session_completed"] and receipt["all_observed_tool_exits_match"]):
                raise ValueError(f"incomplete source receipt: {reference}")
            ready, = [e["state"] for e in receipt["source_setup_and_final_state"] if e["event"] == "source_task_ready"]
            patch_sha = ready.get("debian_patch_archive_sha256")
            if patch_sha:
                pkg = a.cache / "circuit-patch-debs.tar.gz"
                if digest(pkg) != patch_sha:
                    raise ValueError("reviewed Debian patch archive digest mismatch")
                install(pkg,output / pkg.name)
            if curl_sha := ready.get("debian_curl_archive_sha256"):
                pkg = a.cache / "dataset-apt/curl-debs.tar.gz"
                if digest(pkg) != curl_sha:
                    raise ValueError("Debian curl archive differs from the native validation")
                install(pkg, output / pkg.name)
            binary_sha = receipt["adapter_binary_sha256"]
            adapter = output / "adapters" / binary_sha
            if not adapter.exists():
                install(candidates[binary_sha], adapter)
            folder = a.cache / "sources" / task / trial
            target = output / "sources" / task / trial
            for name, key in [("trajectory.json", "trajectory_sha256"), ("rollout.jsonl", "rollout_sha256")]:
                if digest(folder/name) != receipt[key] or item[key] != receipt[key]:
                    raise ValueError(f"source file digest mismatch {trial}/{name}")
                install(folder/name,target/name)
            install(reference,output/"receipts"/reference.name)
            original_logs = [p for p in (a.cache/"logs").glob(f"{task}-{trial}-*.jsonl")
                             if digest(p) == receipt["diagnostic_log_sha256"]]
            raw, = original_logs
            with tempfile.TemporaryDirectory() as folder_name:
                temporary = Path(folder_name)
                os.symlink(raw.resolve(), temporary/"events.jsonl")
                checks = validate(temporary, receipt)
            entries.append({"task":task, "trial_id":trial, "receipt":receipt,
                            "reference_receipt":str(Path("receipts")/reference.name),
                            "unobserved_external_actions":receipt.get("external_actions",[]),
                            "fidelity_scope":"guest_calls_external_responses_unavailable" if receipt.get("external_actions") else "guest_calls",
                            "original_guest_post_checks":checks, "boundary_attribution":boundaries(target/"trajectory.json")})
    (output/"manifest.json").write_text(json.dumps(entries,indent=2)+"\n")
    (output/"pending.json").write_text(json.dumps(pending,indent=2)+"\n")
    (output/"runs").mkdir()
    revision=subprocess.check_output(["git","-C",str(repo),"rev-parse","HEAD"],text=True).strip()
    (output/"revision.txt").write_text(revision+"\n")
    files=sorted(p for p in output.rglob("*") if p.is_file())
    (output/"SHA256SUMS").write_text("".join(f"{digest(p)}  {p.relative_to(output)}\n" for p in files))
    print(f"{output}: {len(entries)} ready, {len(pending)} source-bound pending")


if __name__ == "__main__":
    main()
