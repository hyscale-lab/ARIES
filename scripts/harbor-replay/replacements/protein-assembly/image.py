"""Verify and package the fixed amd64 benchmark image without exporting app payloads."""

import argparse
import hashlib
import io
import json
from pathlib import Path, PurePosixPath
import tarfile

import requests


REPOSITORY = "alexgshaw/protein-assembly"
MANIFEST = "sha256:602c467ae6b661a73441b960d9fa7130f068f4c6041d37ef8632611733d83f4a"
REFERENCE = f"docker.io/{REPOSITORY}@{MANIFEST}"
INPUTS = frozenset({"antibody.fasta", "pdb_ids.txt", "plasmid.gb"})
ROOT = f"https://registry-1.docker.io/v2/{REPOSITORY}"


def sha(data):
    return hashlib.sha256(data).hexdigest()


def encode(obj):
    return json.dumps(obj, sort_keys=True, separators=(",", ":")).encode()


def add(archive, name, data):
    member = tarfile.TarInfo(name)
    member.size = len(data)
    archive.addfile(member, io.BytesIO(data))


def get(http, kind, digest, size=None):
    response = http.get(f"{ROOT}/{kind}/{digest}", timeout=75)
    response.raise_for_status()
    data = response.content
    if f"sha256:{sha(data)}" != digest or (size is not None and len(data) != size):
        raise ValueError(f"registry {kind} failed descriptor validation")
    return data


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    out = parser.parse_args().output
    with requests.Session() as http:
        response = http.get("https://auth.docker.io/token", params={
            "service": "registry.docker.io", "scope": f"repository:{REPOSITORY}:pull",
        }, timeout=10)
        response.raise_for_status()
        http.headers.update({"Authorization": "Bearer " + response.json()["token"],
                             "Accept": "application/vnd.docker.distribution.manifest.v2+json"})
        manifest_bytes = get(http, "manifests", MANIFEST)
        manifest = json.loads(manifest_bytes)
        config = manifest["config"]
        config_bytes = get(http, "blobs", config["digest"], config["size"])
        meta = json.loads(config_bytes)
        if (meta["architecture"], meta["os"], meta["config"]["WorkingDir"]) != ("amd64", "linux", "/app"):
            raise ValueError("unexpected image platform or working directory")
        layers = [(entry["digest"], get(http, "blobs", entry["digest"], entry["size"]))
                  for entry in manifest["layers"]]
    files = {}
    for _, data in layers:
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:*") as layer:
            for member in layer:
                path = PurePosixPath(member.name)
                if path.parts[:1] != ("app",) or len(path.parts) == 1:
                    continue
                if len(path.parts) != 2 or path.name not in INPUTS or not member.isfile():
                    raise ValueError("unexpected entry in the benchmark image's /app")
                files[path.name] = sha(layer.extractfile(member).read())
    if files.keys() != INPUTS:
        raise ValueError("benchmark image's initial file set differs")
    out.mkdir(parents=True, exist_ok=True)
    entry = dict(mediaType=manifest["mediaType"], digest=MANIFEST, size=len(manifest_bytes),
                 platform={"architecture": "amd64", "os": "linux"},
                 annotations={"io.containerd.image.name": REFERENCE,
                              "org.opencontainers.image.ref.name": REFERENCE})
    archive_path = out / "task.oci.tar"
    with tarfile.open(archive_path, "w", format=tarfile.USTAR_FORMAT) as archive:
        add(archive, "index.json", encode(dict(schemaVersion=2, manifests=[entry])))
        add(archive, "oci-layout", encode(dict(imageLayoutVersion="1.0.0")))
        for digest, data in [(MANIFEST, manifest_bytes), (config["digest"], config_bytes), *layers]:
            add(archive, "blobs/sha256/" + digest.removeprefix("sha256:"), data)
    with archive_path.open("rb") as stream:
        archive_sha = hashlib.file_digest(stream, "sha256").hexdigest()
    (out / "image.json").write_text(json.dumps(
        dict(image=REFERENCE, sha256_manifest=MANIFEST, archive_sha256=archive_sha,
             input_sha256=files, created=meta["created"], platform="linux/amd64"),
        sort_keys=True, indent=2) + "\n")


if __name__ == "__main__":
    main()
