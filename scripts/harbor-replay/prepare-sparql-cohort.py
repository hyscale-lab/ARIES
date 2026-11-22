#!/usr/bin/env python3
"""Package original SPARQL trials with successful native fidelity evidence."""
import argparse
import json
from pathlib import Path
import shutil

from cohort_image_circuit import boundaries, digest
from cohort_sparql import CNI_SUMS, CODEX_TOOLS, OS_TOOLS, TASK, verify


def install(src, dst, expected=None):
    if expected and digest(src) != expected:
        raise ValueError(f"unexpected digest for {src}")
    dst.parent.mkdir(parents=True, exist_ok=True)
    shutil.copy2(src, dst)


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("cache", type=Path)
    cli.add_argument("receipts", type=Path)
    cli.add_argument("output", type=Path)
    args = cli.parse_args()
    repo = Path(__file__).resolve().parents[2]
    replay = repo / "scripts/harbor-replay"
    source_cache = args.cache / "sparql"
    output = args.output
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise FileExistsError(f"bundle path must be empty: {output}")
    bound, = [item for item in json.loads((repo / "pkg/replay/harbor/reviewed.json").read_bytes()) if item["task"] == TASK]
    adapters = {digest(p): p for p in source_cache.glob("aries-*") if p.is_file()}
    install(args.cache / "codex-harbor-tools.tar.gz", output / "codex-harbor-tools.tar.gz", CODEX_TOOLS)
    install(source_cache / f"{TASK}-packages.tar.gz", output / f"{TASK}-debs.tar.gz", OS_TOOLS)
    cni = args.cache / "cni"
    install(cni / "SHA256SUMS", output / "cni/SHA256SUMS", CNI_SUMS)
    for row in (cni / "SHA256SUMS").read_text().splitlines():
        sha, filename = row.split()
        install(cni / "bin" / filename, output / "cni/bin" / filename, sha)
    for filename in ("idle_hook.py", "idle.py", "guest_sample.py", "snapshot_probe.py", "sample_vmm.py", "kvm-slots.bt", "vmm_census.py"):
        install(repo / "scripts/zb-trace" / filename, output / "tracer" / filename)
    install(replay / "run-traced.sh", output / "tracer/run-traced.sh")
    for filename in ("run-reviewed-cohort.py", "cohort_image_circuit.py", "cohort_sparql.py", "provision-sparql-cni.sh"):
        install(replay / filename, output / filename)

    ready = []
    pending = []
    for entry in bound["trials"]:
        trial = entry["trial"]
        source = args.cache / "sources" / TASK / trial
        receipt_path = args.receipts / (trial + ".json")
        receipt = json.loads(receipt_path.read_bytes())
        live = receipt.get("replay", {})
        if not (live.get("session_completed") and live.get("all_observed_exits_match")):
            pending.append({"task": TASK, "trial_id": trial, "reason": "successful original live fidelity unavailable"})
            continue
        receipt["task"] = TASK
        for key in ("image", "vcpus", "memory_mib", "storage_mib"):
            if bound[key] != receipt[key]:
                raise ValueError(f"SPARQL reviewed registry differs for {key}")
        if receipt["trial_id"] != trial:
            raise ValueError("SPARQL receipt trial differs")
        binary_sha = live["adapter_binary_sha256"]
        adapter = output / "adapters" / binary_sha
        if not adapter.exists():
            install(adapters[binary_sha], adapter, binary_sha)
        for filename, key in (("trajectory.json", "trajectory_sha256"), ("rollout.jsonl", "rollout_sha256")):
            if receipt[key] != entry[key]:
                raise ValueError(f"SPARQL receipt source identity differs for {key}")
            install(source / filename, output / "sources" / TASK / trial / filename, entry[key])
        logs = [path for path in source_cache.glob(f"{trial}.fidelity-*.jsonl") if digest(path) == live["private_log_sha256"]]
        private_log, = logs
        checks = verify(private_log, receipt)
        reference = Path("receipts") / receipt_path.name
        install(receipt_path, output / reference)
        receipt.update({"session_completed": True, "all_observed_tool_exits_match": True, "adapter_binary_sha256": binary_sha})
        ready.append({"task": TASK, "trial_id": trial, "receipt": receipt, "reference_receipt": str(reference),
                      "original_guest_post_checks": checks, "unobserved_external_actions": receipt["external_actions"],
                      "boundary_attribution": boundaries(source / "trajectory.json")})
    for filename, contents in (("manifest.json", ready), ("pending.json", pending)):
        (output / filename).write_text(json.dumps(contents, indent=2) + "\n")
    (output / "runs").mkdir()
    files = sorted(p for p in output.rglob("*") if p.is_file())
    (output / "SHA256SUMS").write_text("".join(f"{digest(p)}  {p.relative_to(output)}\n" for p in files))
    print(f"{output}: {len(ready)} source-validated, {len(pending)} pending")


if __name__ == "__main__":
    main()
