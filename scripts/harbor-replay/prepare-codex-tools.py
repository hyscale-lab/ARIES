"""Assemble a deterministic guest tool prefix using the source Codex and Node packages."""
import argparse
import gzip
import hashlib
import io
import os
from pathlib import Path
import tarfile
import urllib.request

ROOT = "root/.nvm/versions/node/v22.23.2"
CODEX = ROOT + "/lib/node_modules/@openai/codex"
PLATFORM = CODEX + "/node_modules/@openai/codex-linux-x64"
PATH = "/" + PLATFORM + "/vendor/x86_64-unknown-linux-musl/codex-path"
SOURCE_PATH = ":".join((PATH, PATH, "/" + ROOT + "/bin", "/usr/local/sbin", "/usr/local/bin",
                        "/usr/sbin", "/usr/bin", "/sbin", "/bin"))
PACKAGES = (
    ("https://nodejs.org/dist/v22.23.2/node-v22.23.2-linux-x64.tar.xz",
     "d60acfe00a2932254bb0ad20e01b0d74397a0875595de719654b214f4b03f307", ROOT),
    ("https://registry.npmjs.org/@openai/codex/-/codex-0.151.0.tgz",
     "c332ca76a7b913682b40669f20beefee9ce5c026dd6fdd80f05e282385bf6f68", CODEX),
    ("https://registry.npmjs.org/@openai/codex/-/codex-0.151.0-linux-x64.tgz",
     "b3bcf2c11693d7c8155de637dd6562ba19d916ba13471a7c8737de55e5328fc6", PLATFORM),
)


def make(output, cache):
    with output.open("xb") as raw, gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=0, compresslevel=1) as compressed:
        with tarfile.open(fileobj=compressed, mode="w|", format=tarfile.GNU_FORMAT) as target:
            for url, digest, prefix in PACKAGES:
                source = cache / url.split("/")[-1]
                if not source.exists():
                    urllib.request.urlretrieve(url, source)
                with source.open("rb") as opened:
                    if hashlib.file_digest(opened, "sha256").hexdigest() != digest:
                        raise ValueError(f"incorrect source package hash: {source.name}")
                with tarfile.open(source, mode="r|*") as archive:
                    for member in archive:
                        suffix = member.name.partition("/")[2]
                        member.name = prefix + ("/" + suffix if suffix else "")
                        member.mtime = member.uid = member.gid = 0
                        member.uname = member.gname = ""
                        target.addfile(member, archive.extractfile(member) if member.isfile() else None)
            link = tarfile.TarInfo(ROOT + "/bin/codex")
            link.type = tarfile.SYMTYPE
            link.linkname = "../lib/node_modules/@openai/codex/bin/codex.js"
            link.mode = 0o777
            target.addfile(link)
            # Native calls use the login shell's PATH from the reviewed rollout.
            profile = tarfile.TarInfo("etc/profile.d/99-aries-codex-tools.sh")
            content = f"export PATH='{SOURCE_PATH}'\n".encode()
            profile.mode = 0o644
            profile.size = len(content)
            target.addfile(profile, io.BytesIO(content))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("output", type=Path)
    parser.add_argument("cache", type=Path)
    args = parser.parse_args()
    args.cache.mkdir(parents=True, exist_ok=True)
    tmp = args.output.with_name(args.output.name + ".part")
    try:
        make(tmp, args.cache)
        os.link(tmp, args.output)
    finally:
        tmp.unlink(missing_ok=True)
