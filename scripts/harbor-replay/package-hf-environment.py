"""Package the pinned Python runtime and verified inference wheels for guest setup."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import tarfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("python_archive", type=Path)
parser.add_argument("wheel_dir", type=Path)
parser.add_argument("output", type=Path)
args = parser.parse_args()
manifest = json.loads((args.wheel_dir/"wheels.json").read_text())
with tarfile.open(args.python_archive, "r:gz") as source, args.output.open("xb") as target:
    with gzip.GzipFile(filename="", fileobj=target, mode="wb", mtime=0) as stream:
        with tarfile.open(fileobj=stream, mode="w", format=tarfile.GNU_FORMAT) as out:
            for item in source:
                out.addfile(item, source.extractfile(item) if item.isfile() else None)
            for wheel in manifest["wheels"] + [{"filename":"wheels.json"}]:
                filename = wheel["filename"]
                data = (args.wheel_dir/filename).read_bytes()
                if "sha256" in wheel and hashlib.sha256(data).hexdigest() != wheel["sha256"]:
                    raise ValueError(filename)
                item = tarfile.TarInfo("tmp/aries-hf-setup/" + filename)
                item.size = len(data)
                out.addfile(item, io.BytesIO(data))
print(hashlib.sha256(args.output.read_bytes()).hexdigest())
