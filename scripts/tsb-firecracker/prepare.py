#!/usr/bin/env python3
"""Reconstruct the pinned tracer source, then optionally use Firecracker's build tool."""

import argparse
import hashlib
import json
from pathlib import Path
import subprocess


def git(directory, *args):
    return subprocess.check_output(["git", "-C", str(directory), *args], text=True).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("destination", type=Path, help="new Firecracker source directory")
    parser.add_argument("--source", type=Path,
                        help="existing local Git repository or bundle; otherwise clone upstream")
    parser.add_argument("--build", action="store_true",
                        help="run the native Docker-based tools/devtool build --release")
    args = parser.parse_args()

    directory = Path(__file__).resolve().parent
    versions = json.loads((directory / "versions.json").read_text())
    patch = directory / versions["patch"]
    if hashlib.sha256(patch.read_bytes()).hexdigest() != versions["patch_sha256"]:
        raise ValueError("Firecracker source patch differs from its pinned SHA256")
    destination = args.destination.expanduser().resolve()
    if destination.exists():
        raise FileExistsError("destination must be a new source directory: " + str(destination))
    source = (str(args.source.expanduser().resolve(strict=True)) if args.source
              else versions["upstream_repository"])
    subprocess.run(["git", "clone", "--no-checkout", "--no-hardlinks", source,
                    str(destination)], check=True)
    subprocess.run(["git", "-C", str(destination), "checkout", "--detach",
                    versions["base_revision"]], check=True)
    subprocess.run(["git", "-C", str(destination), "apply", "--index", str(patch)], check=True)
    tree = git(destination, "write-tree")
    if tree != versions["tree"]:
        raise ValueError("reconstructed Firecracker tree differs from its pinned source")
    print("Prepared Firecracker tracer source tree " + tree, flush=True)
    if args.build:
        subprocess.run(["./tools/devtool", "build", "--release"], cwd=destination, check=True)


if __name__ == "__main__":
    main()
