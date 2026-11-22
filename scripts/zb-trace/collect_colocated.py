"""Retrieve completed, counterbalanced Git periods and verify original replay outcomes."""
import argparse
import fcntl
import hashlib
import json
import shlex
import subprocess
import tarfile
import tempfile
from pathlib import Path

if not __debug__:
    raise RuntimeError("source-verification assertions must be enabled")

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument("raw",type=Path,help="private directory for original per-period archives")
parser.add_argument("target",type=Path,help="destination for verified, content-free observations")
parser.add_argument("--host",default="nehalem@amd201.utah.cloudlab.us")
parser.add_argument("--remote",default="/local/zb-texpilot/git-backing/colocated-block-pmem-01")
args=parser.parse_args()
raw,target,host,base=args.raw,args.target,args.host,args.remote
raw.mkdir(parents=True,exist_ok=True)
lock=(raw/"colocated-retrieval.lock").open("a")
fcntl.flock(lock,fcntl.LOCK_EX)
for i,(variant,roles) in enumerate([("v3","a"),("v4","a"),("v3","ab"),("v4","ab"),("v4","ab"),("v3","ab")]):
    key=f"{i:02}-{variant}-{len(roles)}vm"
    if (target/key).exists():continue
    ready=subprocess.run(["ssh",host,"sudo test -s "+base+"/"+key+"/result.json"])
    if ready.returncode==1:break
    ready.check_returncode()
    files=["result.json","host.jsonl"]+[r+"/"+f for r in roles for f in ("events.jsonl","samples.jsonl")]
    archive=raw/("colocated-git-"+key+".tar.gz")
    with archive.with_suffix(".part").open("wb") as output:
        subprocess.run(["ssh",host,"sudo tar czf - -C "+shlex.quote(base+"/"+key)+" "+shlex.join(files)],stdout=output,check=True)
    archive.with_suffix(".part").replace(archive)
    target.mkdir(parents=True,exist_ok=True)
    with tempfile.TemporaryDirectory(dir=target) as staging:
        case=Path(staging)/key
        case.mkdir()
        with tarfile.open(archive) as tf:tf.extractall(case,filter="data")
        meta=json.loads((case/"result.json").read_text())
        assert meta["variant"]==variant and meta["warmup"]==(len(roles)==1)
        hashes={f:hashlib.sha256((case/f).read_bytes()).hexdigest() for f in files}
        evidence=[]
        for j,r in enumerate(roles):
            path=case/r/"events.jsonl"
            original=path.read_bytes()
            events=[json.loads(l) for l in original.splitlines()]
            start=events[0]
            assert start["trajectory_sha256"]=="ec22ca5398af7061a1b51261a7abefdb3775e3422682232fccdaa8494dd22a4f"
            assert start["rollout_sha256"]=="8193dbe7464cf2822e736a524689e69c18ca905db0703c8fc0ee2b716d12e818"
            assert start["trial_id"]==meta["source"] and events[-1]["event"]=="removed"
            tools=[e for e in events if e["event"]=="tool_reply"]
            assert [e["expected_exit_code"] for e in tools]==[0,0,0,1,1,0,0]==[e["exit_code"] for e in tools]
            assert "Ran 27 tests" in tools[-1].get("stdout","")+tools[-1].get("stderr","")
            assert {e["event"]:e["monotonic_ns"] for e in events if e["event"] in meta["session_events"][j]}==meta["session_events"][j]
            evidence.append(dict(role=r,runtime_id=start["runtime_id"],raw_events_sha256=hashlib.sha256(original).hexdigest(),
                                 source_trajectory_sha256=start["trajectory_sha256"],source_rollout_sha256=start["rollout_sha256"],
                                 verified_tool_exits=[e["exit_code"] for e in tools],final_tests=27))
            path.unlink()
        meta.update(verified_replays=evidence,node="amd201",collector_revision="98b1ed9",raw_file_sha256=hashes)
        (case/"result.json").write_text(json.dumps(meta,indent=2)+"\n")
        case.rename(target/key)
        print("verified",key,flush=True)
