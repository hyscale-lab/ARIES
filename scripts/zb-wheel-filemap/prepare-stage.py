#!/usr/bin/env python3
"""Assemble the one-worker v3 experiment from verified kernel, guest root and adapter inputs."""
import argparse
import gzip
import hashlib
from pathlib import Path
import shutil

HERE = Path(__file__).resolve().parent
SCRIPTS = HERE.parent
ADAPTER_SHA = "557158ab56d79ee3f6fca0300f6b0dc423724d4b795adecb5d738a6cb5155541"
CONFIG_SHA = "36a3baa3ad24153ae714d746773d163343c6607e06701c3c2e49f550d7de469d"
REQUIRED_SYMBOLS = {
    "event_mm_filemap_delete_from_page_cache",
    "__tracepoint_mm_filemap_delete_from_page_cache",
}
HELPERS = {
    "audit-idle-runtime.py": SCRIPTS / "harbor-replay",
    "provision_baseline.sh": SCRIPTS / "zb-trace",
    "guest_sample.py": SCRIPTS / "zb-trace",
    "vmm_exclusivity.py": SCRIPTS / "zb-trace",
    "watch-report-replacements.py": SCRIPTS / "zb-demand",
    "reported-replacements.bt": SCRIPTS / "zb-demand",
}


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def prepare(args):
    if digest(args.adapter) != ADAPTER_SHA:
        raise ValueError("The corrected source-clock adapter digest differs")
    kernel = args.kernel_dir
    if digest(kernel / "tracing.config") != CONFIG_SHA:
        raise ValueError("The guest tracing and cachestat configuration differs")
    symbols = {line.split()[-1] for line in (kernel / "deletion-symbols.txt").read_text().splitlines()}
    if symbols != REQUIRED_SYMBOLS:
        raise ValueError("The filemap deletion event symbols differ")
    with gzip.open(args.root_gz, "rb") as stream:
        root_sha = hashlib.file_digest(stream, "sha256").hexdigest()
    if root_sha != args.root_sha:
        raise ValueError("The decompressed diagnostic root digest differs")

    sources = {name: folder / name for name, folder in HELPERS.items()}
    for name in ("run-worker.sh", "cache-observer.py", "trace-release.sh", "replay.py"):
        sources[name] = HERE / name
    for name in ("tracing.vmlinux", "tracing.config", "config-delta.txt", "deletion-symbols.txt"):
        sources[name] = kernel / name
    sources["clock-adapter"] = args.adapter
    sources["diagnostic.ext4.gz"] = args.root_gz
    target = args.stage
    target.mkdir()
    for name, source in sources.items():
        shutil.copy2(source, target / name)
    (target / "runtime-source.sha256").write_text(
        f"{root_sha}  common-rootfs.ext4\n"
        f"{CONFIG_SHA}  common-vmlinux.config\n"
        f"{digest(target / 'tracing.vmlinux')}  common-vmlinux.bin\n"
    )
    inventory = sorted(target.iterdir())
    (target / "SHA256SUMS").write_text("".join(f"{digest(p)}  {p.name}\n" for p in inventory))
    print(f"{digest(target / 'SHA256SUMS')}  {target / 'SHA256SUMS'}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", type=Path)
    parser.add_argument("--kernel-dir", type=Path, required=True)
    parser.add_argument("--root-gz", type=Path, required=True)
    parser.add_argument("--root-sha", required=True, help="SHA-256 of the decompressed fixture image")
    parser.add_argument("--adapter", type=Path, required=True)
    prepare(parser.parse_args())
