"""Attach to the restored stock Firecracker reporting handler during a traced replay."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import time


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream,"sha256").hexdigest()


def start_ticks(proc):
    return int((proc/"stat").read_text().rsplit(")",1)[1].split()[19])


def capture(samples, output, template):
    output.mkdir(mode=0o700)
    deadline=time.monotonic()+360
    while time.monotonic()<deadline:
        if samples.is_file():
            with samples.open() as stream:
                line=stream.readline()
            if line.endswith("\n"):
                break
        time.sleep(.1)
    else:
        raise TimeoutError("No complete restored VMM sample")
    sample=json.loads(line)
    proc=Path("/proc")/str(sample["pid"])
    assert start_ticks(proc)==sample["start_ticks"]
    exe=proc/"exe"
    sha=digest(exe)
    assert sha=="b33f46cd6d88834cf6a4f8d184f08f219809c636236bffd286d233a60602a2ba"
    symbols=subprocess.check_output(["nm","-n",exe],text=True).splitlines()
    address,_,symbol=next(line.split() for line in symbols if len(line.split())==3
                          and line.split()[1].lower()=="t" and line.endswith("process_free_page_reporting_queue"))
    body=template.read_text().replace("VMMPID",str(sample["pid"])).replace("VMMEXE",str(exe)).replace("REPORTSYMBOL",symbol)
    probe=output/"report.bt"
    probe.write_text(body)
    info=dict(pid=sample["pid"],start_ticks=start_ticks(proc),exe_sha256=sha,symbol=symbol,elf_value=address,
              live_inode=exe.stat().st_ino,template_sha256=digest(template),probe_sha256=digest(probe))
    (output/"identity.json").write_text(json.dumps(info,indent=2)+"\n")
    with (output/"events").open("w") as out,(output/"stderr").open("w") as err:
        result=subprocess.run(["bpftrace","-q","-B","line","-p",str(sample["pid"]),probe],stdout=out,stderr=err)
    (output/"exit").write_text(f"{result.returncode}\n")
    result.check_returncode()


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    for name in ("run","output","template"):
        parser.add_argument(name,type=Path)
    parser.add_argument("--samples",type=Path,help="Path to restored VMM samples when stored outside run/trace/")
    args=parser.parse_args()
    capture(args.samples or args.run/"trace/samples.jsonl",args.output,args.template)
