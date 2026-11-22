"""Run the first validated image and circuit sources on both native roots on amd201."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source",type=Path)
    parser.add_argument("output",type=Path)
    parser.add_argument("inventory_sha256")
    args=parser.parse_args()
    source=args.source.resolve(strict=True)
    if hashlib.sha256((source/"SHA256SUMS").read_bytes()).hexdigest()!=args.inventory_sha256:
        raise ValueError("unrecognized source inventory")
    subprocess.run(["sha256sum","--quiet","-c","SHA256SUMS"],cwd=source,check=True)
    entries=json.loads((source/"manifest.json").read_text())
    selected=[min((r for r in entries if r["task"]==task),key=lambda r:r["trial_id"])
              for task in ("code-from-image","circuit-fibsqrt")]
    for turn,(which,variant) in enumerate(((0,"v3"),(0,"v4"),(1,"v4"),(1,"v3"))):
        case=args.output/f"{turn:02}-{variant}"
        case.mkdir(parents=True,exist_ok=True)
        receipt=case/"completed.json"
        if receipt.exists():
            result=json.loads(receipt.read_text())
            if result["inventory_sha256"]!=args.inventory_sha256 or result["trial"]!=selected[which]["trial_id"]:
                raise ValueError("completed period has different inputs")
            continue
        for f in source.iterdir():
            if f.name!="runs" and not (case/f.name).exists():
                (case/f.name).symlink_to(f,target_is_directory=f.is_dir())
        runtime=Path("/local/zb-texpilot")/variant/"runtime"
        unit="fcmod-zepbound-tex-"+variant
        subprocess.run(["systemctl","is-active","--quiet",unit],check=True)
        for comm in Path("/proc").glob("[0-9]*/comm"):
            try:
                name=comm.read_text().strip()
            except (FileNotFoundError,ProcessLookupError):
                continue
            if name in {"firecracker",".firecracker-re"}:
                raise RuntimeError("another Firecracker VM is active")
        entry=selected[which]
        image=entry["receipt"]["image"]
        ctr=[str(runtime/"bin/firecracker-ctr"),"-a",f"/run/{unit}/containerd.sock","-n","aries"]
        subprocess.run([*ctr,"images","pull","--local","--platform","linux/amd64","--snapshotter","devmapper",image],
                       stdout=subprocess.DEVNULL,check=True)
        selection=case/"selected.json"
        selection.write_text(json.dumps([entry])+"\n")
        (case/"runs").mkdir(exist_ok=True)
        subprocess.run(["python3",str(case/"run-reviewed-cohort.py"),str(runtime),unit,str(case),variant,str(selection)],
                       check=True)
        run,=(case/"runs").glob("*-01")
        if (run/"driver.exit").read_text().strip()!="0":
            raise ValueError("direct driver did not complete")
        checks=json.loads((run/"fidelity-checks.json").read_text())
        receipt.write_text(json.dumps(dict(trial=entry["trial_id"],task=entry["task"],variant=variant,
                                       inventory_sha256=args.inventory_sha256,run=run.name,checks=checks),indent=2)+"\n")


if __name__=="__main__":
    main()
