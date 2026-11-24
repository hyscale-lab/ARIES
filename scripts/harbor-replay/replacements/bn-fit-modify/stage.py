"""Seal the five original Bayesian-network sources, adapter and study tracer."""
import argparse
from datetime import datetime
import hashlib
import json
from pathlib import Path
import runpy
import shutil
import subprocess


HERE = Path(__file__).resolve().parent
ARIES = HERE.parents[3]
TASK = "bn-fit-modify"


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def stage(source, index, binary, tracer, out):
    review, = [r for r in json.loads((ARIES / "pkg/replay/harbor/reviewed.json").read_bytes()) if r["task"] == TASK]
    metadata = {r["id"]: r for r in json.loads(index.read_bytes())}
    assert len(metadata) == len(review["trials"]) == 5
    tasks = []
    out.mkdir(mode=0o700)
    (out / "tracer").mkdir()
    (out / "runs").mkdir()
    for row in review["trials"]:
        trial = row["trial"]
        item = metadata[trial]
        assert item["job_id"] == "17d1a7f6-3339-4670-8b70-3b145979f57f" and item["task_name"] == f"terminal-bench/{TASK}"
        duration = datetime.fromisoformat(item["finished_at"]) - datetime.fromisoformat(item["started_at"])
        fields = {"trajectory": "json", "rollout": "jsonl"}
        dest = out / "sources" / trial
        dest.mkdir(parents=True)
        for name, ext in fields.items():
            artifact = source / trial / f"{name}.{ext}"
            assert sha(artifact) == row[f"{name}_sha256"]
            shutil.copyfile(artifact, dest / artifact.name)
        compiled = json.loads(subprocess.check_output([binary, "-trajectory", dest / "trajectory.json",
                                                        "-rollout", dest / "rollout.jsonl"]))
        guest_calls = [e for e in compiled["events"] if e["kind"] == "exec_command"]
        external_calls = [e for e in compiled["events"] if e["kind"] == "web_search_call"]
        assert len(external_calls) == 1 and not compiled["needs_network"] and len(guest_calls) > 0
        tasks.append(row | dict(source_duration_s=duration.total_seconds(), guest_calls=len(guest_calls),
                                recorded_external_searches=len(external_calls), source_reward=item["reward"]))
    tasks.sort(key=lambda r: (r["source_duration_s"], r["trial"]))
    review["trials"] = tasks
    assert sum(r["guest_calls"] for r in tasks) == 47
    trace = runpy.run_path(str(HERE.parents[1] / "deep-expansion/trace-contract.py"))["TRACE"]
    for name, digest in trace.items():
        assert sha(tracer / name) == digest
        shutil.copy2(tracer / name, out / "tracer" / name)
    for name in ("worker.py", "run-worker.sh", "queue.sh"):
        shutil.copyfile(HERE / name, out / name)
    for asset in (HERE.parents[1] / "audit-idle-runtime.py", ARIES / "scripts/zb-trace/vmm_exclusivity.py"):
        shutil.copyfile(asset, out / asset.name)
    shutil.copy2(binary, out / "aries-bn-fit-modify")
    (out / "adapter.sha256").write_text(sha(binary) + "\n")
    cfg = json.loads((source / "pinned-input.json").read_bytes())
    profile = dict(reviewed=review, median_source=tasks[2]["trial"], input=cfg, trace_sha256=trace)
    (out / "source.json").write_text(json.dumps(profile, indent=2) + "\n")
    files = sorted(f for f in out.rglob("*") if f.is_file())
    (out / "SHA256SUMS").write_text("".join(f"{sha(f)}  {f.relative_to(out)}\n" for f in files))
    print(sha(out / "SHA256SUMS"), out)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("source", "index", "binary", "tracer", "out"):
        parser.add_argument(name, type=lambda f: Path(f).resolve())
    stage(**vars(parser.parse_args()))
