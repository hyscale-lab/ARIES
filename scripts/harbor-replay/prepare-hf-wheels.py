"""Fetch the initial CPU inference environment listed by the original Harbor recording."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
from pathlib import Path
from urllib.request import urlopen

from packaging.tags import compatible_tags, cpython_tags
from packaging.utils import parse_wheel_filename


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("trajectory", type=Path)
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    trajectory = json.loads(args.trajectory.read_bytes())
    step = next(step for step in trajectory["steps"] if step["step_id"] == 6)
    table, = [r["content"].split("Package            Version\n")[1].split("\n\n")[0]
              for r in step["observation"]["results"] if "Package            Version\n" in r["content"]]
    versions = dict(line.split() for line in table.splitlines()[1:])
    assert len(versions) == 31 and versions.pop("torch") == "2.7.1+cpu"
    # Torch is pinned by the official benchmark image and checked separately in the guest.
    platforms = []
    for minor in range(36, 4, -1):
        platforms.append(f"manylinux_2_{minor}_x86_64")
        if minor in (17, 12, 5):
            platforms.append({17:"manylinux2014",12:"manylinux2010",5:"manylinux1"}[minor] + "_x86_64")
    tags = [*cpython_tags((3,13), ["cp313"], platforms), *compatible_tags((3,13), "cp313", platforms)]
    rank = {tag:i for i, tag in enumerate(tags)}
    args.directory.mkdir(parents=True, exist_ok=True)

    def fetch(item):
        name, version = item
        with urlopen(f"https://pypi.org/pypi/{name}/{version}/json", timeout=45) as response:
            urls = json.load(response)["urls"]
        options = []
        for obj in urls:
            if obj["packagetype"] != "bdist_wheel":
                continue
            scores = [rank[t] for t in parse_wheel_filename(obj["filename"])[3] if t in rank]
            if scores:
                options.append((min(scores),obj["filename"],obj))
        if not options:
            raise ValueError(f"no supported wheel for {name}=={version}")
        _, filename, obj = min(options)
        target = args.directory / filename
        digest = obj["digests"]["sha256"]
        if not target.exists() or hashlib.sha256(target.read_bytes()).hexdigest() != digest:
            with urlopen(obj["url"], timeout=90) as response:
                data = response.read()
            if hashlib.sha256(data).hexdigest() != digest:
                raise ValueError(f"{filename} digest mismatch")
            target.write_bytes(data)
        return dict(name=name,version=version,filename=filename,sha256=digest,url=obj["url"])

    with ThreadPoolExecutor(6) as pool:
        wheels = list(pool.map(fetch, sorted(versions.items())))
    manifest = dict(source_sha256=hashlib.sha256(args.trajectory.read_bytes()).hexdigest(),
                    image_package={"torch":"2.7.1+cpu"},wheels=wheels)
    (args.directory/"wheels.json").write_text(json.dumps(manifest,indent=2)+"\n")


if __name__ == "__main__":
    main()
