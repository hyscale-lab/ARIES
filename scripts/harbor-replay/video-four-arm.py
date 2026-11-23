#!/usr/bin/env python3
"""Prepare or measure a reviewed video replay on a verified stock study runtime."""
import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tarfile
import tempfile

from cohort_image_circuit import digest
from cohort_video import verify

HERE = Path(__file__).resolve().parent
ARIES = HERE.parents[1]
CACHE = ARIES / ".cache/harbor"
RAW = ARIES / ".cache/zb-raw"
RESULT = ARIES.parents[1] / "plan/result/1_opportunity"
READINESS = RESULT / "replay-validation/video-four-arm"
ARMS = { # worker, runtime, service, existing video CNI, existing large assets
    "v1": ("amd182", "/local/zb-validation/runtime", "fcmod-zepbound-amd182", None, "/local/zb-token-v1-retry-02"),
    "v2": ("amd200", "/local/zb-v2/runtime", "fcmod-zepbound-amd200", "/local/zb-corpus-video-cohort-v2/cni", "/local/zb-corpus-video-cohort-v2"),
    "v3": ("amd189", "/local/zb-stock189-v3/runtime", "fcmod-zepbound-amd189-stockv3", "/local/zb-video189-cni", "/local/zb-video-cohort"),
    "v4": ("amd198", "/local/zb-imagecircuit-v4/runtime", "fcmod-zepbound-amd198-imagecircuit", "/local/zb-imagecircuit-v4/video-assigned-assets/cni", "/local/zb-imagecircuit-v4/video-assigned-assets"),
}
IMAGE = "docker.io/alexgshaw/video-processing@sha256:a2c0f39e3ab04e67ac6e49cad9167bb0d987babe32511768c01aa0c6905a875f"
REVIEW = next(x for x in json.loads((ARIES/"pkg/replay/harbor/reviewed.json").read_bytes()) if x["task"] == "video-processing")
assert REVIEW["image"] == IMAGE
def checked(*args, **kwargs):
    return subprocess.check_output(list(map(str, args)), **kwargs)

def upload(host, files, target):
    subprocess.run(["scp", "-rq", *map(str,files), host+":"+target+"/"],check=True)

def remote(host, *args, **kw):
    return checked("ssh", "-o", "ConnectTimeout=12", host, shlex.join(list(map(str,args))), **kw)

def sources():
    candidates = []
    for trial in REVIEW["trials"]:
        folder = CACHE/"sources/video-processing"/trial["trial"]
        if digest(folder/"rollout.jsonl") != trial["rollout_sha256"] or digest(folder/"trajectory.json") != trial["trajectory_sha256"]:
            raise ValueError("video source hash differs")
        atif = json.loads((folder/"trajectory.json").read_bytes())
        calls = [c["function_name"] for step in atif["steps"] for c in step.get("tool_calls",[])]
        candidates.append(dict(**trial, image_observations=calls.count("view_image"),
                               guest_calls=sum(n in {"view_image","write_stdin","exec_command"} for n in calls)))
    return candidates


def write_readiness(variant, files):
    node, runtime, unit, cni, _ = ARMS[variant]
    old = {"v1":"amd189-v1-idle-runtime", "v2":"amd183-stock-v2-deployment",
           "v3":"amd189-stock-v3-hf-deployment", "v4":"amd189-stock-v3-hf-deployment"}[variant]
    donor = json.loads((RESULT/"replay-validation"/(old+".json")).read_bytes())
    template = donor.get("runtime_readback", donor)
    readback = json.loads((files/"deployment-readback.json").read_bytes())
    profile = json.loads((files/"effective-profile.json").read_bytes())
    network = json.loads((files/"network.json").read_bytes())
    if readback["node"] != node or readback["unit"].removesuffix(".service") != unit:
        raise ValueError("worker deployment identity differs")
    for k in ("firecracker","rootfs","kernel","shim","daemon","runc","ctr"):
        if readback["files"][k]["sha256"] != template["files"][k]["sha256"]:
            raise ValueError(f"{variant} runtime mismatch: {k}")
    if (profile["smt_enabled"] is not False or profile["guest_root_mode"] != "disk"
            or bool(profile.get("root_drive_use_pmem")) != (variant=="v4")
            or bool(profile.get("free_page_reporting")) != (variant=="v2")
            or bool((profile.get("guest_reclamation") or {}).get("Enabled")) != (variant in ("v3","v4"))):
        raise ValueError("unexpected effective Firecracker profile")
    if (network["cni"]["name"] != "zbvideo"+node[3:]
            or set(network["tls"]) != {"pypi.org", "files.pythonhosted.org"}
            or (files/"task-image.txt").read_text().strip() != IMAGE):
        raise ValueError("video environment CNI or OCI image mismatch")
    old_run = RESULT/"runs"/f"07e979df-{variant}-cohort-01/run.json"
    prior = json.loads(old_run.read_bytes())
    revisions = dict(prior["revisions"])
    for k in ("aries_cli","aries_cli_sha256","cohort_driver_sha256","cohort_inventory_sha256"):
        revisions.pop(k,None)
    if revisions["aries_hook_sha256"] != digest(ARIES/"scripts/zb-trace/idle_hook.py"):
        raise ValueError("survey hook differs from admitted video original")
    outcome = dict(schema="aries.harbor.video.cohort.preparation.v1",variant=variant,node=node,runtime=runtime,unit=unit,
                   snapshot_mode=prior["snapshot_mode"],revisions=revisions,runtime_readback=readback,
                   effective_profile=profile,network=network,image=IMAGE,vcpus=REVIEW["vcpus"],
                   storage_mib=REVIEW["storage_mib"],memory_mib=REVIEW["memory_mib"],trial_driver_sha256=digest(HERE/"run-cohort-trials.py"),
                   source_review=sources(),source_review_file_sha256=digest(ARIES/"pkg/replay/harbor/reviewed.json"),
                   preflight_sha256=digest(HERE/"prepare-video-worker.sh"),bpftrace=(files/"bpftrace-version.txt").read_text().strip())
    READINESS.mkdir(parents=True,exist_ok=True)
    (READINESS/(variant+".json")).write_text(json.dumps(outcome,indent=2)+"\n")


def prepare(variant):
    node,runtime,unit,cni,_=ARMS[variant]
    host = f"nehalem@{node}.utah.cloudlab.us"
    stage = f"/local/zb-video-source-supplemental-{variant}"
    remote(host,"bash","-c","if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 3; fi")
    remote(host,"sudo","-n","install","-d","-m","0755","-o","nehalem",stage)
    upload(host,[HERE/n for n in ("audit-idle-runtime.py","provision-pypi-cni.sh","prepare-video-worker.sh","run-cohort-trials.py")],stage)
    if cni is None:
        cni=stage+"/cni"
        remote(host,"mkdir","-p",cni)
        upload(host,[CACHE/"video-cohort/cni/bin",CACHE/"video-cohort/cni/SHA256SUMS"],cni)
    remote(host,"bash",stage+"/prepare-video-worker.sh",stage,runtime,unit,cni,variant,node,IMAGE)
    with tempfile.TemporaryDirectory() as tmp:
        path=Path(tmp)
        for name in ("deployment-readback.json","effective-profile.json","network.json","task-image.txt","bpftrace-version.txt"):
            (path/name).write_bytes(remote(host,"cat",stage+"/"+name))
        write_readiness(variant,path)


def trial_for(prefix):
    matches = [t for t in REVIEW["trials"] if t["trial"].startswith(prefix)]
    if len(prefix) < 8 or len(matches) != 1:
        raise ValueError("supply a unique reviewed original trial UUID or its eight-character prefix")
    return matches[0]

def bundle_for(t):
    """Seal the exact source validation before any remote measurement request."""
    ident = t["trial"]
    receipts = RESULT/"replay-validation"
    strict = receipts/f"video-processing-{ident}-strict.json"
    if not strict.is_file():
        strict = receipts/f"video-processing-{ident}.json"
    if not strict.is_file(): raise ValueError("native strict source validation pending")
    receipt = json.loads(strict.read_bytes())
    if not (receipt["session_completed"] and receipt["all_observed_tool_exits_match"] and receipt["all_observed_image_payloads_match"]
            and not receipt.get("diagnostic_video_images") and not receipt.get("diagnostic_process_drift") and not receipt["observer"]):
        raise ValueError("full uninstrumented strict validation with exact image bytes is pending")
    dest = CACHE/f"video-cohort-{ident[:8]}-{digest(strict)[:12]}"
    if not dest.exists():
        with tempfile.TemporaryDirectory(dir=CACHE,prefix="video-seal-") as tmp:
            build = Path(tmp)/"bundle"
            checked(sys.executable,HERE/"prepare-image-circuit-cohort.py",CACHE,receipts,build,"--video","--trial",ident)
            try:build.rename(dest)
            except OSError:
                if not dest.exists(): raise
    checked("sha256sum","--quiet","-c","SHA256SUMS",cwd=dest)
    entry, = json.loads((dest/"manifest.json").read_bytes())
    if entry["receipt"]!=receipt or digest(dest/entry["reference_receipt"])!=digest(strict):
        raise ValueError("sealed reference receipt differs")
    for filename,key in (("rollout.jsonl","rollout_sha256"),("trajectory.json","trajectory_sha256")):
        if digest(dest/"sources/video-processing"/ident/filename)!=t[key]:raise ValueError("source input hash differs")
    return dest, entry

def launch(variant,t,bundle):
    node,runtime,unit,cni,previous = ARMS[variant]
    host = f"nehalem@{node}.utah.cloudlab.us"
    stage = f"/local/zb-video-source-supplemental-{variant}"
    dest = stage+"/"+bundle.name
    key=t["trial"][:8]+"-"+variant+"-cohort-01"
    if (RESULT/"runs"/key).exists():raise FileExistsError("inspect existing canonical cell: "+key)
    state=json.loads((READINESS/(variant+".json")).read_bytes())
    if state["node"]!=node or state["variant"]!=variant or state["image"]!=IMAGE:raise ValueError("incorrect deployment metadata")
    # The staged runtime config and exact helper are rechecked inside the worker lock.
    remote(host,"bash","-c","if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 3; fi")
    remote(host,"mkdir","-p",dest)
    subprocess.run(["rsync","-rltp","--checksum","--exclude=/runs/","--copy-dest="+previous,str(bundle)+"/",host+":"+dest+"/"],check=True)
    script="""set -euo pipefail
cd "$1"
exec 9>"$2/measure.lock"
flock -n 9
if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 3; fi
test ! -e "runs/$3"
test "$(sha256sum SHA256SUMS | cut -d' ' -f1)" = "$4"
sha256sum --quiet -c SHA256SUMS
test "$(sha256sum "$5/runtime.json" | cut -d' ' -f1)" = "$6"
test "$(sha256sum "$2/run-cohort-trials.py" | cut -d' ' -f1)" = "$7"
mkdir -p runs
sudo -n python3 -B "$2/run-cohort-trials.py" "$5" "$8" "$1" "$9" manifest.json --video-cni "${10}"
"""
    remote(host,"bash","-s","--",dest,stage,key,digest(bundle/"SHA256SUMS"),runtime,state["runtime_readback"]["files"]["runtime_config"]["sha256"],state["trial_driver_sha256"],unit,variant,cni or stage+"/cni",input=script.encode())

def collect(variant,t,bundle,entry):
    node,*_=ARMS[variant]; host=f"nehalem@{node}.utah.cloudlab.us"
    dest=f"/local/zb-video-source-supplemental-{variant}/{bundle.name}/runs"
    key=t["trial"][:8]+"-"+variant+"-cohort-01"
    rows=[json.loads(l) for l in remote(host,"sudo","-n","cat",dest+"/cohort-"+variant+"-status.jsonl").splitlines()]
    row,=[x for x in rows if x["trial"]==t["trial"] and x["variant"]==variant]
    if row["manifest_sha256"]!=digest(bundle/"manifest.json") or row["driver_sha256"]!=digest(bundle/"run-reviewed-cohort.py"):
        raise ValueError("completed driver status differs from sealed bundle")
    archive=RAW/(key+".tar.gz")
    with tempfile.NamedTemporaryFile(dir=RAW) as tmp:
        subprocess.run(["ssh",host,shlex.join(["sudo","-n","tar","czf","-","-C",dest,key])],stdout=tmp,check=True)
        tmp.flush()
        if archive.exists() and digest(archive)!=digest(Path(tmp.name)):
            raise ValueError("existing raw archive differs")
        raw=RAW/key
        if not raw.exists():
            with tempfile.TemporaryDirectory(dir=RAW,prefix="video-unpack-") as unpack:
                with tarfile.open(tmp.name) as tar:
                    for member in tar:
                        if Path(member.name).parts[0]!=key or not (member.isfile() or member.isdir()):
                            raise ValueError("unexpected remote archive")
                    tar.extractall(unpack,filter="data")
                (Path(unpack)/key).rename(raw)
        if not archive.exists():
            os.link(tmp.name,archive)
    if int((raw/"driver.exit").read_text())!=row["driver_exit"]:raise ValueError("driver status mismatch")
    if row["driver_exit"]:raise RuntimeError("raw failed attempt retained at "+str(raw))
    verify(raw,entry["receipt"])
    deployed=READINESS/(variant+".json")
    m=json.loads(checked(sys.executable,HERE/"network-measurement-metadata.py",raw,bundle,deployed))
    m.update(cohort_completion=row,raw_archive_sha256=digest(archive),worker_deployment_sha256=digest(deployed),
             worker_deployment_receipt=deployed.relative_to(RESULT).as_posix(),boundary_attribution=entry["boundary_attribution"])
    meta=raw/"metadata.json";meta.write_text(json.dumps(m,indent=2)+"\n")
    target=RESULT/"runs"/key
    if not target.exists():checked(sys.executable,ARIES/"scripts/zb-trace/process_run.py",raw,meta,target)
    actual=json.loads((target/"run.json").read_bytes())
    if actual["status"]!="validated" or actual["raw_archive_sha256"]!=m["raw_archive_sha256"]:raise ValueError("canonical result mismatch")
    print(target)

if __name__=="__main__":
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("action",choices=("prepare","seal","run","collect"));p.add_argument("trial",help="worker variant for prepare, source UUID or prefix otherwise");p.add_argument("variant",choices=ARMS,nargs="?")
    a=p.parse_args()
    if a.action=="prepare":
        if a.trial not in ARMS or a.variant is not None:p.error("prepare takes a single variant")
        prepare(a.trial);sys.exit(0)
    if (a.action=="seal") != (a.variant is None):p.error("supply a variant for run or collect")
    t=trial_for(a.trial);b,e=bundle_for(t)
    if a.action=="seal":print(b)
    elif a.action=="run":launch(a.variant,t,b)
    else:collect(a.variant,t,b,e)
