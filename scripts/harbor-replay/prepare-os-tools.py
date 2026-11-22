"""Build the pinned offline package archive for a reviewed Ubuntu or Debian guest."""
import argparse
import hashlib
import shutil
from pathlib import Path
import subprocess
import tempfile
import urllib.parse

HERE = Path(__file__).resolve().parent / "packages"


def assemble(task, output, package_dir=None):
    entries = [line.split() for line in (HERE / f"{task}-packages.sha256").read_text().splitlines()]
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        requests = []
        names = []
        for _, filename in entries:
            package, version, _ = urllib.parse.unquote(filename).split("_")
            requests.append(f"{package}={version}")
            names.append(filename)
        if package_dir is None:
            subprocess.run(["apt-get", "download", *requests], cwd=directory, check=True)
        else:
            for filename in names:
                source = package_dir / filename
                if not source.exists():
                    source = package_dir / urllib.parse.unquote(filename)
                shutil.copy2(source, directory / filename)
        for sha, filename in entries:
            path = directory / filename
            if not path.exists():
                (directory / urllib.parse.unquote(filename)).rename(path)
            with path.open("rb") as f:
                if hashlib.file_digest(f, "sha256").hexdigest() != sha:
                    raise ValueError(f"package digest differs for {filename}")
        # Shell glob order used for the original bundles was C collation order.
        tar = directory / "packages.tar"
        subprocess.run(["tar", "--sort=name", "--mtime=@0", "--owner=0", "--group=0",
                        "--numeric-owner", "-cf", tar, *sorted(names)], cwd=directory, check=True)
        with output.open("xb") as f:
            level = "-6" if task == "bookworm-patch" else "-1"
            subprocess.run(["gzip", "-n", level, "-c", tar], stdout=f, check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("task", choices=("regex-log", "constraints-scheduling", "overfull-hbox", "sparql-university", "compile-compcert", "dataset-bookworm-curl", "bookworm-patch"))
    parser.add_argument("output", type=Path)
    parser.add_argument("--package-dir", type=Path, help="retained amd64 packages checked against the same pinned hashes")
    args = parser.parse_args()
    assemble(args.task, args.output, args.package_dir)
