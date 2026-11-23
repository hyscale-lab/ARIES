"""Reconstruct the task's WORKDIR layer over the pinned Python version."""
import argparse
import gzip
import hashlib
from io import BytesIO
import json
from pathlib import Path
import tarfile

import requests


BASE = "sha256:3e2de9c40ca4e3d73240059f9d48baff27908f10293e985a2f382a0378e6df4a"
LISTED = "sha256:84c7fae6b256dcc56a350790e2a9715eefc7dad662a9d8e8a472363aa71ef18d"
PYTHON = "3.13.15"
REPO = "library/python"
REGISTRY = f"https://registry-1.docker.io/v2/{REPO}"
IMAGE_SPEC = "https://raw.githubusercontent.com/harbor-framework/terminal-bench-2-1/12b7565679ce5006230b820666353673a133db93/tasks/cancel-async-tasks/environment/Dockerfile"
PREFIX = "application/vnd.oci.image."


def encode(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def descriptor(data, media):
    return dict(mediaType=PREFIX + media, digest=digest(data), size=len(data))


def add(archive, path, data):
    info = tarfile.TarInfo(path)
    info.size = len(data)
    info.mode = 0o644
    archive.addfile(info, BytesIO(data))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.mkdir()
    with requests.Session() as http:
        recipe = http.get(IMAGE_SPEC, timeout=25)
        recipe.raise_for_status()
        assert [line for line in recipe.text.splitlines() if line and not line.startswith("#")] == [
            "FROM python:3.13-slim-bookworm", "WORKDIR /app"]
        token = http.get("https://auth.docker.io/token",
                         params={"service": "registry.docker.io", "scope": f"repository:{REPO}:pull"},
                         timeout=20)
        token.raise_for_status()
        http.headers.update({"Authorization": "Bearer " + token.json()["token"],
                             "Accept": PREFIX + "manifest.v1+json"})

        def retrieve(kind, expected, size=None):
            response = http.get(f"{REGISTRY}/{kind}/{expected}", timeout=80)
            response.raise_for_status()
            body = response.content
            assert digest(body) == expected
            assert size is None or len(body) == size
            return body

        base = json.loads(retrieve("manifests", BASE))
        original = json.loads(retrieve("blobs", base["config"]["digest"], base["config"]["size"]))
        assert (original["architecture"], original["os"]) == ("amd64", "linux")
        assert f"PYTHON_VERSION={PYTHON}" in original["config"]["Env"]
        assert original["config"].get("WorkingDir", "") in {"", "/"}
        for entry in base["layers"]:
            assert entry["mediaType"] == PREFIX + "layer.v1.tar+gzip"
        workdir = BytesIO()
        with tarfile.open(fileobj=workdir, mode="w", format=tarfile.USTAR_FORMAT) as layer:
            app = tarfile.TarInfo("app")
            app.type = tarfile.DIRTYPE
            app.mode = 0o755
            layer.addfile(app)
        app_tar = workdir.getvalue()
        app_gzip = gzip.compress(app_tar, mtime=0)
        layer = descriptor(app_gzip, "layer.v1.tar+gzip")
        original["rootfs"]["diff_ids"].append(digest(app_tar))
        original["config"]["WorkingDir"] = "/app"
        original["history"].append({"created_by": "WORKDIR /app"})
        config = encode(original)
        layers = [*base["layers"], layer]
        manifest = encode(dict(schemaVersion=2, mediaType=PREFIX + "manifest.v1+json",
                               config=descriptor(config, "config.v1+json"), layers=layers))
        index_entry = descriptor(manifest, "manifest.v1+json")
        reference = f"localhost/zepbound/cancel-async-tasks:py31315-{index_entry['digest'][7:19]}"
        index_entry["platform"] = {"architecture": "amd64", "os": "linux"}
        index_entry["annotations"] = {
            "io.containerd.image.name": reference,
            "org.opencontainers.image.ref.name": reference,
        }
        index = encode(dict(schemaVersion=2, manifests=[index_entry]))
        archive = args.output / "task.oci.tar"
        with tarfile.open(archive, "w", format=tarfile.USTAR_FORMAT) as out:
            add(out, "oci-layout", encode({"imageLayoutVersion": "1.0.0"}))
            add(out, "index.json", index)
            for body in (manifest, config, app_gzip):
                add(out, "blobs/sha256/" + digest(body)[7:], body)
            for entry in base["layers"]:
                sha = entry["digest"]
                add(out, "blobs/sha256/" + sha[7:], retrieve("blobs", sha, entry["size"]))
    with archive.open("rb") as stream:
        archive_sha = hashlib.file_digest(stream, "sha256").hexdigest()
    (args.output/"image.json").write_text(json.dumps(dict(
        image=reference, sha256_manifest=index_entry["digest"], archive_sha256=archive_sha,
        base_manifest=BASE, listed_task_image_manifest=LISTED, base_config=base["config"]["digest"],
        task_dockerfile=IMAGE_SPEC, task_dockerfile_sha256=digest(recipe.content), base_created=original["created"],
        observed_python_version=PYTHON, historical_base_digest_observed=False,
    ), indent=2)+"\n")


if __name__ == "__main__":
    main()
