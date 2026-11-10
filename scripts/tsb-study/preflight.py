#!/usr/bin/env python3
"""Freeze requested research inputs; this never certifies runtime admission."""
import argparse
import hashlib
import json
import pathlib
import subprocess
import tomllib

SOURCES = {
    "mock": "profiles/vhive/openclaw-tb2-fix-git-echo-vhive.json",
    "fix-git": "profiles/vhive/openclaw-tb2-fix-git-deepseek-vhive.json",
    "five": "profiles/openclaw-tb2-five-deepseek.json",
    "twenty": "profiles/experiments/openclaw-tb2-twenty-deepseek.json",
}
PIN = "7131e4375048a0e408a8fb404b5f499d726b695b"
def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def preflight(root):
    versions = json.loads((root / "configs/versions.json").read_text())
    if versions["terminalbench2"]["revision"] != PIN:
        raise ValueError("benchmark pin changed")
    rows = []
    for stage, source in SOURCES.items():
        source = root / source
        source_cfg = json.loads(source.read_text())
        profile = root / "profiles/experiments/tsb-memory-footprint" / (stage + ".json")
        cfg = json.loads(profile.read_text())
        overrides_path = (profile.parent / cfg["overrides_file"]).resolve()
        overrides = json.loads(overrides_path.read_text())
        if cfg["benchmark"]["tasks"] != source_cfg["benchmark"]["tasks"]:
            raise ValueError(stage + ": task membership or order changed")
        if cfg["execution"]["concurrency"] != 1:
            raise ValueError(stage + ": concurrency must be one")
        if overrides["agent_sandbox_resources"] != {"cpu": 2, "memory_mb": 4096}:
            raise ValueError(stage + ": uniform sandbox capacity violated")
        deploy = cfg["sandbox"]["deployment"]
        vm = deploy["vhive"]
        if deploy["backend"] != "vhive" or (vm["idle_mode"], vm["vcpus"], vm["memory_mb"]) != ("persistent", 2, 4096):
            raise ValueError(stage + ": invalid deployment")
        if stage != "mock" and (cfg["model"]["id"],cfg["model"]["reasoning_effort"]) != ("deepseek-flash","high"):
            raise ValueError(stage + ": user-selected Flash/high configuration changed")
        benchmark = root / cfg["benchmark"]["root"]
        actual_pin = subprocess.check_output(["git","-C",str(benchmark),"rev-parse","HEAD"],text=True).strip()
        if actual_pin != PIN:
            raise ValueError("benchmark checkout does not match pin")
        tasks = []
        for name in cfg["benchmark"]["tasks"]:
            task_path = benchmark / "tasks" / name / "task.toml"
            task = tomllib.loads(task_path.read_text())
            env = task["environment"]
            blockers = []
            if env.get("gpus",0): blockers.append("GPU unsupported by vHive")
            if env.get("storage_mb",0) not in (0,10240): blockers.append("worker devmapper capacity mismatch")
            if env.get("mcp_servers"): blockers.append("in-sandbox services require routing audit")
            tasks.append({
                "task":name,"task_sha256":digest(task_path),
                "image":env.get("docker_image"),"declared_resources":{k:env.get(k) for k in ["cpus","memory_mb","storage_mb","gpus"]},
                "requested_resources":{"cpu":2,"memory_mb":4096,"storage_mb":env.get("storage_mb")},
                "allow_internet":env.get("allow_internet"),
                "agent_deadline_seconds":overrides.get("agent_timeout_seconds",task["agent"]["timeout_sec"]),
                "verifier_deadline_seconds":task["verifier"]["timeout_sec"],
                "build_deadline_seconds":env.get("build_timeout_sec"),
                "static_blockers":blockers,
                "runtime_image_utilities_network_and_capacity":"not_verified_by_static_preflight",
            })
        rows.append({
            "stage":stage,"source_profile":str(source.relative_to(root)),"source_sha256":digest(source),
            "profile":str(profile.relative_to(root)),"profile_sha256":digest(profile),
            "override_sha256":digest(overrides_path),"tasks":tasks,
            "nominal_real_attempts":0 if stage=="mock" else len(tasks),
            "retry_cap":5 if stage=="mock" else 2,
            "agent_deadline_sum_seconds":sum(t["agent_deadline_seconds"] for t in tasks),
            "verifier_deadline_sum_seconds":sum(t["verifier_deadline_seconds"] for t in tasks),
            "trace_storage_limit_bytes":20*1024**3,
            "admission":"requires calibration, complete causal Step evidence and prior-stage gate",
        })
    return {"schema_version":1,"benchmark_pin":PIN,"versions_sha256":digest(root/"configs/versions.json"),
            "source_baseline_commit":"89b48a7f9174592bb71538bb5f0da3d46cc0dd26",
            "code_commit":subprocess.check_output(["git","-C",str(root),"rev-parse","HEAD"],text=True).strip(),
            "scope":"static request preflight; not execution or measurement evidence","stages":rows}

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--root",type=pathlib.Path,default=pathlib.Path("."))
    ap.add_argument("--output",type=pathlib.Path,required=True)
    args=ap.parse_args()
    result=preflight(args.root.resolve())
    with args.output.open("x") as f: json.dump(result,f,indent=2);f.write("\n")
    for stage in result["stages"]:
        print(stage["stage"],len(stage["tasks"]),"tasks; agent deadline sum",stage["agent_deadline_sum_seconds"],"s; verifier",stage["verifier_deadline_sum_seconds"],"s")
if __name__=="__main__": main()
