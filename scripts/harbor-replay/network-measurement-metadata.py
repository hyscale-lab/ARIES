"""Verify a traced network-dependent session and attach its deployed runtime lineage."""
import argparse
import hashlib
import json
from pathlib import Path

import cohort_dataset
import cohort_image_circuit
import cohort_pypi
import cohort_sparql
import cohort_video


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def metadata(raw, bundle, deployment):
    lineage = json.loads(deployment.read_text())
    if (raw/"driver.exit").read_text().strip() != "0":
        raise ValueError("worker driver failed")
    for filename in ("idle.jsonl","slots.log","samples.jsonl"):
        if (raw/filename).stat().st_size == 0:
            raise ValueError(f"missing measurement: {filename}")
    log = raw/"events.jsonl"
    events = [json.loads(line) for line in log.read_bytes().splitlines()]
    first = events[0]
    entry, = [e for e in json.loads((bundle/"manifest.json").read_text())
              if (e["task"],e["trial_id"]) == (first["task"],first["trial_id"])]
    task, trial = entry["task"], entry["trial_id"]
    if task in cohort_pypi.TASKS:
        reference = cohort_pypi.receipt_path(task,trial)
        receipt = cohort_pypi.load_receipt(bundle,entry)
        apt = entry.get("financial_apt")
        hf = entry.get("hf")
        prefix = "zbhfm" if hf else ("zbfinanceapt" if apt else "zbpypi")
        if task == cohort_pypi.MARKER:
            prefix = "zbmarker"
        network = prefix+lineage["node"].removeprefix("amd")
        checks = cohort_pypi.verify(log,receipt,entry["reference_boundaries"],network,apt,hf)
    else:
        reference = entry["reference_receipt"]
        receipt = entry["receipt"]
        if task in cohort_image_circuit.TASKS:
            checks = cohort_image_circuit.verify(raw,receipt)
        elif task in cohort_sparql.PROFILES:
            checks = cohort_sparql.verify(log,receipt)
        elif task == "count-dataset-tokens":
            checks = cohort_dataset.verify(raw,receipt)
        elif task == cohort_video.TASK:
            checks = cohort_video.verify(raw,receipt)
        else:
            raise ValueError(f"no network measurement verifier for {task}")
    if checks != json.loads((raw/"fidelity-checks.json").read_text()):
        raise ValueError("local verification differs from worker")
    sha = receipt["adapter_binary_sha256"]
    if digest(bundle/"adapters"/sha) != sha:
        raise ValueError("packaged source adapter digest differs")
    for key in ("trajectory","rollout"):
        path = bundle/"sources"/task/trial/(key+(".json" if key=="trajectory" else ".jsonl"))
        if digest(path) != first[key+"_sha256"]:
            raise ValueError("source bytes differ")
    deployment_keys = ("aries_hook","aries_hook_sha256","runtime","stock_firecracker","guest_kernel","root_ext4_sha256")
    revisions = {k:lineage["revisions"][k] for k in deployment_keys}
    revisions.update(aries_cli=receipt.get("replay",receipt)["adapter_revision"],
                     aries_cli_sha256=sha,cohort_driver_sha256=digest(bundle/"run-reviewed-cohort.py"),
                     cohort_inventory_sha256=digest(bundle/"SHA256SUMS"))
    if revisions["aries_hook_sha256"] != digest(bundle/"tracer/idle_hook.py"):
        raise ValueError("trace hook differs from deployment")
    final, = [e for e in events if e["event"]=="final_task_state"]
    if not raw.name.startswith(trial[:8]+"-"+lineage["variant"]+"-"):
        raise ValueError("variant or source differs from run label")
    return dict(run_id=raw.name,workload=task,variant=lineage["variant"],node=lineage["node"],
                source_trial=trial,job_id=first["job_id"],status="validated",snapshot_id=None,
                snapshot_mode=lineage["snapshot_mode"],revisions=revisions,
                source_validation_receipt=str(reference),source_validation_sha256=digest(bundle/reference),
                final_checks=final["checks"],memory_mib=first["memory_mib"],storage_mib=first["storage_mib"],
                vcpus=first["vcpus"],fidelity=checks)


if __name__ == "__main__":
    p=argparse.ArgumentParser(description=__doc__)
    for key in ("raw","bundle","deployment"):
        p.add_argument(key,type=Path)
    a=p.parse_args()
    print(json.dumps(metadata(a.raw,a.bundle,a.deployment),indent=2))
