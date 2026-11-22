#!/usr/bin/env python3
"""Run independent manifest entries with a pinned driver and retain each result."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tempfile


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("runtime")
    p.add_argument("unit")
    p.add_argument("assets", type=Path)
    p.add_argument("variant", choices=("v1","v2","v3","v4"))
    p.add_argument("manifest", type=Path)
    p.add_argument("driver_options", nargs=argparse.REMAINDER)
    a=p.parse_args()
    manifest=a.manifest.read_bytes()
    entries=json.loads(manifest)
    driver=a.assets/"run-reviewed-cohort.py"
    lineage={"manifest_sha256":hashlib.sha256(manifest).hexdigest(),
             "driver_sha256":hashlib.sha256(driver.read_bytes()).hexdigest()}
    status=a.assets/"runs"/("cohort-"+a.variant+"-status.jsonl")
    status.parent.mkdir(exist_ok=True)
    failed=0
    with tempfile.TemporaryDirectory(prefix="aries-reviewed-") as tmp, status.open("a") as out:
        selection=Path(tmp)/"trial.json"
        for entry in entries:
            selection.write_text(json.dumps([entry])+"\n")
            result=subprocess.run([sys.executable,driver,a.runtime,a.unit,a.assets,a.variant,selection,*a.driver_options])
            row=dict(**lineage,task=entry["task"],trial=entry["trial_id"],variant=a.variant,driver_exit=result.returncode)
            out.write(json.dumps(row)+"\n")
            out.flush()
            if result.returncode:
                failed+=1
                # A crashed driver can leave a VM on this dedicated measurement worker.
                # Preserve that state for diagnosis before starting another measurement.
                running=subprocess.run(["pgrep","-x","firecracker"],stdout=subprocess.DEVNULL)
                if running.returncode != 1:
                    raise RuntimeError("cannot establish a clean worker after failed trial "+entry["trial_id"])
    return int(failed!=0)


if __name__ == "__main__":
    sys.exit(main())
