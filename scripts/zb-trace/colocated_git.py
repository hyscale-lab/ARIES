"""Compare two original Git replays on native block-root and pmem stacks on the same worker."""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import subprocess
import threading
import time

TRIAL = "02a77c58-1e9a-4cfb-a332-da5a752e0cfc"
IMAGE = "docker.io/alexgshaw/fix-git@sha256:389b9c8247610c2c5be080b1ac00429007c2c69bf57f7f26c79f0f75ba2d5c74"
INPUTS = {"02a77c58.json":"ec22ca5398af7061a1b51261a7abefdb3775e3422682232fccdaa8494dd22a4f",
          "02a77c58.jsonl":"8193dbe7464cf2822e736a524689e69c18ca905db0703c8fc0ee2b716d12e818"}


def host_sample(groups):
    begin=time.monotonic_ns()
    meminfo={}
    for line in Path("/proc/meminfo").read_text().splitlines():
        key,val,*unit=line.split()
        if unit==["kB"]:
            meminfo[key.rstrip(":")]=int(val)*1024
    charges={}
    for unit,group in groups.items():
        stat=dict(line.split() for line in (group/"memory.stat").read_text().splitlines())
        charges[unit]=dict(current=int((group/"memory.current").read_text()),
                           **{k:int(stat[k]) for k in ("anon","file","kernel","pagetables","slab") if k in stat})
    return dict(begin_ns=begin,end_ns=time.monotonic_ns(),host_bytes=meminfo,cgroups_bytes=charges)


def sample_until(stop, path, groups):
    with path.open("x") as out:
        while not stop.is_set():
            start=time.monotonic()
            out.write(json.dumps(host_sample(groups))+"\n")
            out.flush()
            stop.wait(max(0,1-(time.monotonic()-start)))


def run_vm(assets, cli, runtime, unit, output):
    output.mkdir(mode=0o700)
    (output/"io").mkdir(mode=0o700)
    args=[cli,"-trajectory",assets/"02a77c58.json","-rollout",assets/"02a77c58.jsonl","-image",IMAGE,
          "-native-checkpoint","-namespace","aries","-socket","/run/"+unit+"/containerd.sock",
          "-checkpoint-dir",runtime/"checkpoints","-shim-dir",runtime/"shim","-io-dir",output/"io",
          "-ripgrep",assets/"rg","-sampler",assets/"tracer/sample_vmm.py","-samples",output/"samples.jsonl",
          "-log",output/"events.jsonl"]
    with (output/"stderr").open("x") as err:
        subprocess.run(args,check=True,stderr=err,stdout=subprocess.DEVNULL)
    ev=[json.loads(line) for line in (output/"events.jsonl").read_text().splitlines()]
    replies=[e for e in ev if e["event"]=="tool_reply"]
    if (ev[0]["trial_id"]!=TRIAL or ev[-1]["event"]!="removed" or [r["exit_code"] for r in replies]!=[0,0,0,1,1,0,0]
            or not all(r["exit_code"]==r["expected_exit_code"] for r in replies) or "Ran 27 tests" not in (replies[-1]["stdout"]+replies[-1].get("stderr",""))):
        raise RuntimeError("Git source outcomes differed")
    return {e["event"]:e["monotonic_ns"] for e in ev if e["event"] in
            ("create_start","restore_complete","session_start","session_end","removed")}


def main():
    p=argparse.ArgumentParser(description=__doc__)
    for name in ("assets","runtime_base","output"):
        p.add_argument(name,type=Path)
    p.add_argument("v3_unit");p.add_argument("v4_unit")
    a=p.parse_args()
    assets=a.assets.resolve(strict=True)
    cli=assets/"aries-d1cba28"
    for filename,sha in INPUTS.items():
        if hashlib.sha256((assets/filename).read_bytes()).hexdigest()!=sha:
            raise ValueError(filename)
    output=a.output.resolve()
    output.mkdir(mode=0o700)
    units={"v3":a.v3_unit,"v4":a.v4_unit}
    groups={}
    for variant,unit in units.items():
        group=subprocess.check_output(["systemctl","show","-p","ControlGroup","--value",unit],text=True).strip()
        if not group.startswith("/system.slice/"):
            raise ValueError("expected dedicated service cgroup")
        groups[variant]=Path("/sys/fs/cgroup")/group.lstrip("/")
        runtime=a.runtime_base/variant/"runtime"
        env=dict(os.environ)
        env.pop("ZB_CHECKPOINT_NAMESPACE_DIR",None)
        subprocess.run([runtime/"bin/firecracker-ctr","-a","/run/"+unit+"/containerd.sock","-n","aries","images","pull",
                        "--local","--platform","linux/amd64","--snapshotter","devmapper",IMAGE],env=env,check=True,stdout=subprocess.DEVNULL)
    os.environ.pop("ZB_CHECKPOINT_NAMESPACE_DIR",None)
    for index,(variant,n) in enumerate((("v3",1),("v4",1),("v3",2),("v4",2),("v4",2),("v3",2))):
        if subprocess.run(["pgrep","-x","firecracker"],stdout=subprocess.DEVNULL).returncode!=1:
            raise RuntimeError("worker has another running VMM")
        case=output/f"{index:02d}-{variant}-{n}vm"
        case.mkdir()
        stop=threading.Event()
        with concurrent.futures.ThreadPoolExecutor(3) as pool:
            sampler=pool.submit(sample_until,stop,case/"host.jsonl",groups)
            try:
                time.sleep(20)
                futures=[pool.submit(run_vm,assets,cli,a.runtime_base/variant/"runtime",units[variant],case/"a")]
                if n==2:
                    time.sleep(40)
                    futures.append(pool.submit(run_vm,assets,cli,a.runtime_base/variant/"runtime",units[variant],case/"b"))
                sessions=[f.result() for f in futures]
                time.sleep(15)
            finally:
                stop.set()
            sampler.result()
        meta=dict(variant=variant,source=TRIAL,warmup=(n==1),session_events=sessions,
                  cli_sha256=hashlib.sha256(cli.read_bytes()).hexdigest(),service_cgroups={k:str(v) for k,v in groups.items()})
        (case/"result.json").write_text(json.dumps(meta,indent=2)+"\n")


if __name__=="__main__":
    main()
