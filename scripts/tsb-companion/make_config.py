#!/usr/bin/env python3
"""Write an explicit companion config only for a hash-matching supplied kernel ELF."""
import argparse
import hashlib
import json
from pathlib import Path

def build_config(kernel,layouts):
    kernel=Path(kernel).resolve();layouts=Path(layouts).resolve()
    data=kernel.read_bytes()
    if not data.startswith(b"\x7fELF"):raise ValueError("supply the actual guest kernel ELF")
    digest=hashlib.sha256(data).hexdigest()
    files={"layout_path":layouts/"base-layout.json","owner_layout_path":layouts/"owner-layout.json",
           "names_layout_path":layouts/"names-layout.json"}
    for name,path in files.items():
        if json.loads(path.read_text())["kernel_sha256"]!=digest:
            raise ValueError("kernel ELF does not match "+name+"; generate and validate matching layouts")
    return {"kernel_image":str(kernel),**{k:str(v) for k,v in files.items()},
            "enable_resident_sampling":True,"resident_sample_pages":1024,"enable_compression":False}

def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kernel-elf",required=True,type=Path)
    parser.add_argument("--layouts",type=Path,default=Path(__file__).parent/"layouts/linux-5.10.239")
    parser.add_argument("--output",required=True,type=Path)
    args=parser.parse_args()
    value=build_config(args.kernel_elf,args.layouts)
    with args.output.open("x") as stream:json.dump(value,stream,indent=2);stream.write("\n")

if __name__=="__main__":main()
