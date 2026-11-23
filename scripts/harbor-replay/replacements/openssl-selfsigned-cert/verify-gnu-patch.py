"""Bind the Debian GNU patch archive to the observed certificate source behavior."""
import argparse
import hashlib
import json
import lzma
from pathlib import Path
import subprocess
import tarfile
import tempfile


def digest(data):
    return hashlib.sha256(data).hexdigest()


def verify(release, packages, keyring, archive, source):
    subprocess.run(["gpgv", "--keyring", keyring.resolve(), release], check=True)
    index = packages.read_bytes()
    lines = release.read_text().splitlines()
    signed = next(l.split() for l in lines[lines.index("SHA256:"):] if l.endswith(" main/binary-amd64/Packages.xz"))
    assert signed == [digest(index), str(len(index)), "main/binary-amd64/Packages.xz"]
    paragraphs = lzma.decompress(index).decode().split("\n\n")
    entry = next(dict(l.split(": ", 1) for l in p.splitlines() if ": " in l)
                 for p in paragraphs if p.startswith("Package: patch\n"))
    assert (entry["Version"], entry["Architecture"]) == ("2.7.6-7", "amd64")
    with tarfile.open(archive) as stored:
        member, = [m for m in stored if m.isfile()]
        with stored.extractfile(member) as stream:
            deb = stream.read()
    assert digest(deb) == entry["SHA256"]
    with source.open() as stream:
        calls = {i: json.loads(line)["payload"] for i, line in enumerate(stream, 1) if i in (66, 72)}
    with tempfile.TemporaryDirectory(prefix="zb-gnu-patch-check-") as folder:
        local = Path(folder)
        (local / "patch.deb").write_bytes(deb)
        subprocess.run(["dpkg-deb", "-x", local / "patch.deb", local / "package"], check=True)
        patch = [local / "package/usr/bin/patch", "--batch", "--forward", "-p0"]
        for line, expected in ((66, 1), (72, 0)):
            command = json.loads(calls[line]["arguments"])["cmd"]
            diff = command.split("apply_patch <<'PATCH'\n", 1)[1].split("\nPATCH\n", 1)[0] + "\n"
            argv = patch + (["--dry-run"] if line == 66 else [])
            outcome = subprocess.run(argv, input=diff, cwd=local, text=True, capture_output=True)
            assert outcome.returncode == expected
            text = outcome.stdout + outcome.stderr
            assert ("Ignoring potentially dangerous file name /app/check_cert.py" if line == 66
                    else "patching file check_cert.py") in text
        assert (local / "check_cert.py").is_file()
    return dict(release_sha256=digest(release.read_bytes()), packages_xz_sha256=digest(index),
                release_keyring_sha256=digest(keyring.read_bytes()), deb_sha256=digest(deb),
                archive_sha256=digest(archive.read_bytes()), source_rollout_sha256=digest(source.read_bytes()),
                distro="Debian Bookworm", version=entry["Version"],
                source_behavior={"line_66": "absolute filename rejected, exit 1",
                                 "line_72": "relative filename creates checker, exit 0"})


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("release", "packages", "keyring", "archive", "source", "output"):
        p.add_argument(name, type=Path)
    a = p.parse_args()
    result = verify(a.release, a.packages, a.keyring, a.archive, a.source)
    a.output.write_text(json.dumps(result, indent=2) + "\n")
