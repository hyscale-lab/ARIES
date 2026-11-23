#!/usr/bin/env python3
"""Extract a TLS bootstrap bundle from the digest-bound Ubuntu certificate package."""
import argparse
import gzip
import hashlib
import io
from pathlib import Path
import subprocess
import tarfile
import tempfile

DEB = "ca-certificates_20260816ubuntu0.24.04.2_all.deb"
SHA = "f9fbacd417b515450b831032cc39d18059e9874e67525679a43c16071dabf9a0"


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("existing_compiler_archive", type=Path)
    cli.add_argument("output", type=Path)
    args = cli.parse_args()
    with tarfile.open(args.existing_compiler_archive) as archive:
        deb = archive.extractfile(DEB).read()
    if hashlib.sha256(deb).hexdigest() != SHA:
        raise ValueError("Ubuntu CA package differs from verified archive")
    with tempfile.TemporaryDirectory() as dirname:
        path = Path(dirname) / DEB
        path.write_bytes(deb)
        tar = subprocess.check_output(["dpkg-deb", "--fsys-tarfile", path])
    with tarfile.open(fileobj=io.BytesIO(tar)) as bundle:
        names = sorted(m.name for m in bundle if m.isfile()
                       and "/share/ca-certificates/mozilla/" in m.name and m.name.endswith(".crt"))
        if not names:
            raise ValueError("missing public CA store")
        content = b"".join(bundle.extractfile(n).read().rstrip() + b"\n" for n in names)
    with args.output.open("xb") as target, gzip.GzipFile(
        filename="", mode="wb", compresslevel=6, fileobj=target, mtime=0
    ) as compressed, tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as out:
        entry = tarfile.TarInfo("aries-compiler-ca.pem")
        entry.size = len(content)
        entry.mode = 0o644
        out.addfile(entry, io.BytesIO(content))


if __name__ == "__main__":
    main()
