"""Build the original gRPC trial's Python and pip toolchain from a pinned public layer."""
import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import tarfile
from urllib.request import Request, urlopen

# Official python:3.13.15-slim-bookworm linux/amd64, manifest 3e2de9c40ca4…
LAYER = "2e57034c052c6d059f36df62c9e6f4ee3630bb3a653a5f9e4a196159d53abb35"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    parser.add_argument("--layer", type=Path, help="previously downloaded official blob")
    args = parser.parse_args()
    if args.layer:
        blob = args.layer.read_bytes()
    else:
        auth = "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/python:pull"
        token = json.load(urlopen(auth, timeout=30))["token"]
        url = "https://registry-1.docker.io/v2/library/python/blobs/sha256:" + LAYER
        blob = urlopen(Request(url, headers={"Authorization":"Bearer "+token}), timeout=90).read()
    if hashlib.sha256(blob).hexdigest() != LAYER:
        raise ValueError("official Python layer digest differs")
    with tarfile.open(fileobj=io.BytesIO(blob)) as source, args.output.open("xb") as target:
        with gzip.GzipFile(filename="", fileobj=target, mode="wb",mtime=0) as zipped:
            with tarfile.open(fileobj=zipped, mode="w", format=tarfile.GNU_FORMAT) as out:
                for m in source:
                    if m.name.startswith("usr/local/"):
                        out.addfile(m, source.extractfile(m) if m.isfile() else None)
    print(hashlib.sha256(args.output.read_bytes()).hexdigest())


if __name__ == "__main__":
    main()
