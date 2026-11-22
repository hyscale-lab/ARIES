"""Build the pinned offline tool package archive for a reviewed Ubuntu guest."""
import argparse
import hashlib
from pathlib import Path
import subprocess
import tempfile
import urllib.parse

HERE = Path(__file__).resolve().parent / "packages"


def assemble(task, output):
    entries = [line.split() for line in (HERE / f"{task}-packages.sha256").read_text().splitlines()]
    with tempfile.TemporaryDirectory() as tmp:
        directory = Path(tmp)
        requests = []
        names = []
        for _, filename in entries:
            package, version, _ = urllib.parse.unquote(filename).split("_")
            requests.append(f"{package}={version}")
            names.append(filename)
        subprocess.run(["apt-get", "download", *requests], cwd=directory, check=True)
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
            subprocess.run(["gzip", "-n", "-1", "-c", tar], stdout=f, check=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("task", choices=("regex-log", "constraints-scheduling", "overfull-hbox"))
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    assemble(args.task, args.output)
