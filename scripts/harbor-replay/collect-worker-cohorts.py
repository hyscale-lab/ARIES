"""Reconcile completed, immutable cohort driver results from the primary workers."""
import argparse
import concurrent.futures
import fcntl
import hashlib
import json
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tarfile

if not __debug__:
    raise RuntimeError("source verification requires assertions")
parser=argparse.ArgumentParser(description=__doc__)
for name in ("raw","bundles","runs"):
    parser.add_argument(name,type=Path)
args=parser.parse_args()
raw,bundles,runs=(p.resolve() for p in (args.raw,args.bundles,args.runs))
raw.mkdir(parents=True,exist_ok=True)
scripts=Path(__file__).resolve().parents[1]

def digest(path):
    with path.open("rb") as f:
        return hashlib.file_digest(f,"sha256").hexdigest()
def collect(pair):
    variant,node=pair
    host=f"nehalem@{node}.utah.cloudlab.us"
    cmd=f"sudo bash -c 'for f in /local/zb-corpus-*-{variant}/runs/cohort-{variant}-status.jsonl; do if test -f \"$f\"; then sed \"s|^|$f |\" \"$f\"; fi; done'"
    lines=subprocess.check_output(["ssh",host,cmd],text=True).splitlines()
    for line in lines:
        name,row=line.split(" ",1)
        result=json.loads(row)
        if not re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}",result["trial"]):
            raise ValueError("malformed trial ID in completed cohort")
        key=result["trial"][:8]+"-"+variant+"-cohort-01"
        if (runs/key/"run.json").is_file():
            existing=json.loads((runs/key/"run.json").read_bytes())
            if existing["source_trial"]!=result["trial"] or existing["node"]!=node:
                raise ValueError("run key already belongs to a different placement or original")
            continue
        remote=Path(name).parent
        bundle=bundles/remote.parent.name.removeprefix("zb-corpus-").removesuffix("-"+variant)
        assert digest(bundle/"manifest.json")==result["manifest_sha256"] and digest(bundle/"run-reviewed-cohort.py")==result["driver_sha256"]
        entry,=[e for e in json.loads((bundle/"manifest.json").read_text()) if e["trial_id"]==result["trial"]]
        archive=raw/(key+".tar.gz")
        with archive.with_suffix(".part").open("wb") as out:
            subprocess.run(["ssh",host,"sudo tar czf - -C "+shlex.quote(str(remote))+" "+shlex.quote(key)],stdout=out,check=True)
        archive.with_suffix(".part").replace(archive)
        with tarfile.open(archive) as tar:
            for member in tar:
                parts=Path(member.name).parts
                if not parts or parts[0]!=key or not (member.isfile() or member.isdir()):
                    raise ValueError("unexpected file in completed trial archive")
            tar.extractall(raw,filter="data")
        folder=raw/key
        deployment=runs/("036e7370-"+variant+"-cohort-01")/"run.json"
        assert json.loads(deployment.read_text())["node"]==node
        if result["driver_exit"]==0:
            meta=json.loads(subprocess.check_output([sys.executable,scripts/"harbor-replay/network-measurement-metadata.py",folder,bundle,deployment]))
        else:
            events=[json.loads(e) for e in (folder/"events.jsonl").read_bytes().splitlines()]
            start=events[0]
            ref=entry.get("reference_receipt",f"receipts/{entry['task']}-{result['trial']}.json")
            receipt=json.loads((bundle/ref).read_text())
            source=entry.get("receipt",receipt)
            assert start["trial_id"]==result["trial"] and start["task"]==entry["task"]
            for k,fn in (("trajectory","trajectory.json"),("rollout","rollout.jsonl")):
                assert start[k+"_sha256"]==digest(bundle/"sources"/entry["task"]/result["trial"]/fn)
            mismatch=[dict(step=e["step_id"],sequence=e["sequence"],actual=e["exit_code"],source=e["expected_exit_code"])
                      for e in events if e["event"]=="tool_reply" and e["exit_code"]!=e["expected_exit_code"]]
            meta=dict(run_id=key,workload=entry["task"],variant=variant,node=node,source_trial=result["trial"],job_id=start["job_id"],
                      status="partial_fidelity",snapshot_mode=json.loads(deployment.read_text())["snapshot_mode"],failed_source_statuses=mismatch,
                      driver_exit=result["driver_exit"],source_validation_receipt=ref,source_validation_sha256=digest(bundle/ref),
                      source_trajectory_sha256=start["trajectory_sha256"],source_rollout_sha256=start["rollout_sha256"],image=start["image"],
                      revisions=dict(aries_cli_sha256=source["adapter_binary_sha256"],cohort_inventory_sha256=digest(bundle/"SHA256SUMS")),
                      raw_sha256={f.name:digest(f) for f in folder.iterdir() if f.is_file() and f.name!="metadata.json"})
        if "worker_profile_amendment" in result:
            meta["worker_profile_amendment"]=result["worker_profile_amendment"]
        (folder/"metadata.json").write_text(json.dumps(meta,indent=2)+"\n")
        subprocess.run([sys.executable,scripts/"zb-trace/process_run.py",folder,folder/"metadata.json",runs/key],check=True)
        print(node,key,meta["status"],flush=True)
with (raw/"collect-bundles.lock").open("w") as lock:
    fcntl.flock(lock,fcntl.LOCK_EX)
    with concurrent.futures.ThreadPoolExecutor(3) as pool:
        list(pool.map(collect,[("v1","amd203"),("v2","amd200"),("v3","amd201")]))
