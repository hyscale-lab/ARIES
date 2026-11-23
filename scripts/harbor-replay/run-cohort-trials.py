#!/usr/bin/env python3
"""Run independent manifest entries with a pinned driver and retain each result."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def worker_driver(assets, unit, entries, temporary):
    driver=assets/"run-reviewed-cohort.py"
    tasks={entry["task"] for entry in entries}
    if unit == "fcmod-zepbound-amd189-stockv3" and tasks == {"hf-model-inference"}:
        helper = assets / "cohort_hf.py"
        original = helper.read_bytes()
        if hashlib.sha256(original).hexdigest() != "bea188baeda0a30565d901dee5a50614aaa74237e896b6c15b2b7e51ad55dd62":
            raise ValueError("unexpected scoped inference source helper")
        for module in assets.glob("*.py"):
            shutil.copyfile(module, temporary / module.name)
        prefix = b'    bridge, firewall, redirect, loopback = config["plugins"]\n'
        assert original.count(prefix) == 1 and original.count(b'f"10.235.{match[1]}') == 2
        updated = original.replace(prefix, b'    subnet_octet = 237 if name == "zbhfm189" else 235\n' + prefix)
        updated = updated.replace(b'f"10.235.{match[1]}', b'f"10.{subnet_octet}.{match[1]}')
        (temporary / helper.name).write_bytes(updated)
        return temporary / driver.name, dict(module=helper.name, source_sha256=hashlib.sha256(original).hexdigest(), deployed_sha256=hashlib.sha256(updated).hexdigest(), network="zbhfm189", source_subnet="10.235.189.0/28", deployed_subnet="10.237.189.0/28")
    pypi183 = unit=="fcmod-zepbound-amd183-stockv2" and tasks <= {"kv-store-grpc","financial-document-processor"}
    finance201 = unit=="fcmod-zepbound-tex-v3" and tasks=={"financial-document-processor"} and all(e.get("financial_apt") for e in entries)
    if pypi183 or finance201:
        if finance201:
            name,profile,node,old_octet,new_octet = "zbfinanceapt201","financeapt",201,234,237
            expected = "3b0f0c610650f0284d1ef2245f29a3018952864d0e472997d7635c9e50475015"
        else:
            name,profile,node,old_octet,new_octet = "zbpypi183","pypi",183,233,236
            expected = "31597d7497db2924e2b8b1eff66a2cd6f25cd1c0fcffe8fc80f87fb68ed15bbb"
        helper = assets / "cohort_pypi.py"
        original = helper.read_bytes()
        if hashlib.sha256(original).hexdigest() != expected:
            raise ValueError("unexpected scoped PyPI source helper")
        for module in assets.glob("*.py"):
            shutil.copyfile(module, temporary / module.name)
        target = b'    name = config["name"]\n'
        assert original.count(target) == 1
        replacement = target + f'    if name == "{name}" and profile == "{profile}":\n        network = {new_octet}\n'.encode()
        updated = original.replace(target, replacement)
        (temporary / helper.name).write_bytes(updated)
        return temporary / driver.name, dict(module=helper.name, source_sha256=hashlib.sha256(original).hexdigest(), deployed_sha256=hashlib.sha256(updated).hexdigest(), network=name, source_subnet=f"10.{old_octet}.{node}.0/28", deployed_subnet=f"10.{new_octet}.{node}.0/28")
    if unit!="fcmod-zepbound-amd200" or not tasks.intersection({"count-dataset-tokens","video-processing"}):
        return driver, {}
    helper=assets/"cohort_dataset.py"
    old=helper.read_bytes()
    old_nodes=b"(182|183|189|191|198|201|203)"
    new_nodes=b"(182|183|189|191|198|200|201|203)"
    if new_nodes in old:
        return driver, {}
    # Sealed packages predate amd200 in the original worker list. Keep every
    # semantic check and the original bytes, updating only the exact name list.
    if old.count(old_nodes)!=1:
        raise ValueError("unrecognized worker allowlist")
    for module in assets.glob("*.py"):
        shutil.copyfile(module,temporary/module.name)
    updated=old.replace(old_nodes,new_nodes)
    (temporary/helper.name).write_bytes(updated)
    return temporary/driver.name, dict(module=helper.name,source_sha256=hashlib.sha256(old).hexdigest(),
                                       deployed_sha256=hashlib.sha256(updated).hexdigest(),added_worker="amd200")


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
        driver, amendment=worker_driver(a.assets,a.unit,entries,Path(tmp))
        if amendment:
            lineage["worker_profile_amendment"]=amendment
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
                running=subprocess.run(["pgrep","-x","firecracker|[.]firecracker-re"],stdout=subprocess.DEVNULL)
                if running.returncode != 1:
                    raise RuntimeError("cannot establish a clean worker after failed trial "+entry["trial_id"])
    return int(failed!=0)


if __name__ == "__main__":
    sys.exit(main())
