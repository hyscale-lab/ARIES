#!/usr/bin/env python3
"""Package validated gRPC and financial trials, their exact adapters and private CNI assets."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil

import cohort_hf
import cohort_pypi
from cohort_image_circuit import IMAGE_HELPER, digest
from cohort_pypi import (CNI_SUMS, CODEX_TOOLS, FINANCIAL, GRPC, IMAGE_HELPER_FILE, OS_TOOLS,
                         PYTHON_ARCHIVE, TASKS, boundaries, events_from, pinned, receipt_path, verify)


def install(src, dst, expected=None):
    if expected is not None:
        pinned(src, expected)
    dst.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dst)
    if expected is not None:
        pinned(dst, expected)


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("cache", type=Path)
    cli.add_argument("receipts", type=Path)
    cli.add_argument("output", type=Path)
    profile = cli.add_mutually_exclusive_group()
    profile.add_argument("--financeapt", action="store_true", help="package only signed-snapshot financial trials")
    profile.add_argument("--hf", action="store_true", help="package only model-inference trials")
    cli.add_argument("--trial",help="package only the named, source-reviewed trial")
    cli.add_argument("--validation-log", type=Path, action="append", required=True,
                     help="original live diagnostic JSONL named by a validated receipt digest (repeatable)")
    args = cli.parse_args()

    repo = Path(__file__).resolve().parents[2]
    replay = repo / "scripts/harbor-replay"
    bound = json.loads((repo / "pkg/replay/harbor/reviewed.json").read_bytes())
    apt = json.loads((repo / "cmd/aries-harbor/financial_apt.json").read_bytes())
    hf_path = args.cache / "hf-fixture/environment.tar.gz"
    if args.hf:
        hf_trials = json.loads((repo / "cmd/aries-harbor/hf_trials.json").read_bytes())
        hf_environment = cohort_hf.source_environment(hf_path)
    logs = {digest(p): p for p in args.validation_log}
    prefixes = ("aries-hf*",) if args.hf else ("aries-kv*", "aries-fin*")
    adapters = {digest(p): p for prefix in prefixes for p in args.cache.glob(prefix)
                if p.is_file()}
    ready, pending = [], []
    for reviewed in bound:
        task = reviewed["task"]
        if task not in TASKS or args.hf != (task == cohort_hf.TASK):
            continue
        if not reviewed["needs_network"]:
            raise ValueError(f"{task} lacks source-bound network requirements")
        for entry in reviewed["trials"]:
            trial = entry["trial"]
            if args.trial and trial != args.trial:
                continue
            financial_apt = apt.get(trial)
            if args.financeapt != (financial_apt is not None):
                continue
            ident = dict(task=task, trial_id=trial)
            path = args.receipts / receipt_path(task, trial).name
            if not path.is_file():
                pending.append(dict(ident, reason="source-specific validation receipt unavailable"))
                continue
            receipt_bytes = path.read_bytes()
            receipt = json.loads(receipt_bytes)
            if (not receipt["session_completed"] or not receipt["all_observed_tool_exits_match"] or
                    receipt.get("diagnostic_process_drift") or receipt.get("diagnostic_video_images") or receipt.get("diagnostic_grpc_loopback") or receipt.get("primary_eligible") is False):
                pending.append(dict(ident, reason="successful original live fidelity unavailable"))
                continue
            log = logs.get(receipt["diagnostic_log_sha256"])
            if log is None:
                pending.append(dict(ident, reason="named validation log digest unavailable"))
                continue
            for aliases in (ident, {key: reviewed[key] for key in ("image", "vcpus", "memory_mib", "storage_mib")},
                            {key: entry[key] for key in ("trajectory_sha256", "rollout_sha256")}):
                if any(receipt[k] != expected for k, expected in aliases.items()):
                    raise ValueError(f"reviewed identity differs from the native receipt for {trial}")
            reference = boundaries(events_from(log))
            hf = dict(hf_trials[trial], **hf_environment) if args.hf else None
            checks = verify(log, receipt, reference, financial_apt=financial_apt, hf=hf)
            initial, = [e["state"] for e in receipt["source_setup_and_final_state"] if e["event"] == "source_task_ready"]
            if initial["codex_prefix_sha256"] != CODEX_TOOLS:
                raise ValueError(f"source Codex tool archive differs for {trial}")
            if task == GRPC and receipt.get("python_archive_sha256", PYTHON_ARCHIVE) != PYTHON_ARCHIVE:
                raise ValueError(f"source Python archive differs for {trial}")
            if task == FINANCIAL and initial["ubuntu_packages_sha256"] != OS_TOOLS:
                raise ValueError(f"source Ubuntu package archive differs for {trial}")
            sha = receipt["adapter_binary_sha256"]
            if sha not in adapters:
                raise FileNotFoundError(f"source-validated adapter missing for {trial}: {sha}")
            ready.append((ident, path, hashlib.sha256(receipt_bytes).hexdigest(), receipt, reference, checks, financial_apt, hf))
    if not ready:
        raise ValueError("no reviewed trial has a successful receipt and matching native log")

    out = args.output
    out.mkdir(mode=0o700, parents=True, exist_ok=True)
    if any(out.iterdir()):
        raise FileExistsError(f"bundle path must be empty: {out}")
    install(args.cache / "codex-harbor-tools.tar.gz", out / "codex-harbor-tools.tar.gz", CODEX_TOOLS)
    cases = {ident["task"] for ident, *_ in ready}
    if GRPC in cases:
        install(args.cache / "kv-fixture/kv-python.tar.gz", out / "kv-python.tar.gz", PYTHON_ARCHIVE)
    if any(ident["trial_id"] == cohort_pypi.GRPC_LOOPBACK_TRIAL for ident, *_ in ready):
        assets = args.cache / "grpc69ba-assets"
        install(assets / "grpc-git-patch.tar.gz",out / "grpc-git-patch.tar.gz",cohort_pypi.GRPC_SYSTEM_TOOLS_SHA)
        install(assets / "package-lineage.json",out / "package-lineage.json")
        for name in ("run-grpc-primary.py","network-measurement-metadata.py","cohort_dataset.py","cohort_video.py","audit-idle-runtime.py"):
            install(replay / name,out / name)
    if FINANCIAL in cases:
        install(args.cache / "sparql/sparql-university-packages.tar.gz", out / f"{FINANCIAL}-debs.tar.gz", OS_TOOLS)
        install(args.cache / IMAGE_HELPER_FILE, out / IMAGE_HELPER_FILE, IMAGE_HELPER)
    if cohort_hf.TASK in cases:
        install(hf_path, out / cohort_hf.ENVIRONMENT, cohort_hf.ENVIRONMENT_SHA)
        for filename in ("cohort_hf.py", "provision-pypi-cni.sh"):
            install(replay / filename, out / filename)
    cni = args.cache / "cni"
    cni_sums = pinned(cni / "SHA256SUMS", CNI_SUMS)
    install(cni_sums, out / "cni/SHA256SUMS", CNI_SUMS)
    for sha, filename in map(str.split, cni_sums.read_text().splitlines()):
        install(cni / "bin" / filename, out / "cni/bin" / filename, sha)
    for filename in ("idle_hook.py", "idle.py", "guest_sample.py", "snapshot_probe.py", "sample_vmm.py", "kvm-slots.bt", "vmm_census.py"):
        install(repo / "scripts/zb-trace" / filename, out / "tracer" / filename)
    install(replay / "run-traced.sh", out / "tracer/run-traced.sh")
    for filename in ("run-reviewed-cohort.py", "cohort_image_circuit.py", "cohort_sparql.py", "cohort_pypi.py", "provision-sparql-cni.sh"):
        install(replay / filename, out / filename)

    manifest = []
    for ident, path, receipt_sha, receipt, reference, checks, financial_apt, hf in ready:
        task, trial = ident["task"], ident["trial_id"]
        sha = receipt["adapter_binary_sha256"]
        adapter = out / "adapters" / sha
        if not adapter.is_file():
            install(adapters[sha], adapter, sha)
        relative = Path("sources") / task / trial
        for filename, key in (("trajectory.json", "trajectory_sha256"), ("rollout.jsonl", "rollout_sha256")):
            install(args.cache / relative / filename, out / relative / filename, receipt[key])
        install(path, out / receipt_path(task, trial), receipt_sha)
        measured = dict(ident, receipt_sha256=receipt_sha, reference_boundaries=reference,
                        native_fidelity_checks=checks, unobserved_external_actions=receipt["external_actions"])
        if financial_apt is not None:
            measured["financial_apt"] = financial_apt
        if hf is not None:
            measured["hf"] = hf
        manifest.append(measured)
    (out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (out / "pending.json").write_text(json.dumps(pending, indent=2) + "\n")
    (out / "runs").mkdir()
    files = sorted(p for p in out.rglob("*") if p.is_file())
    (out / "SHA256SUMS").write_text("".join(f"{digest(p)}  {p.relative_to(out)}\n" for p in files))
    print(f"{out}: {len(manifest)} source-validated, {len(pending)} pending")


if __name__ == "__main__":
    main()
