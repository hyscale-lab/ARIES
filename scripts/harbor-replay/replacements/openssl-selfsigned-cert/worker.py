"""Launch one pinned certificate recording on an idle study worker."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


TASK = "openssl-selfsigned-cert"
ROOT = "292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f"
WIDE_ROOT = "8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5"
CODEX = "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab"
TRACE = {
    "guest_sample.py": "e61390b021c54c201e117c1da0c181370abe17687b1b0888bbb42124b50114e9",
    "idle.py": "568100daa47324e5a84b7cfed961607ac8ac6ddeb023bf0acc379fe5e7266dd9",
    "idle_hook.py": "aa388a611e2743273e75a67b4fe7b95a7cc3d027c808686d2c03c6736cf4b06e",
    "kvm-slots.bt": "ffd610ffed7241e17dbd1636c5a12294bc51f377f8bb24f645f3d33866a9515a",
    "run-traced.sh": "ee9a984b325468a45e471aafb176875e75166d4e806f2102de35b110cc8e925d",
    "sample_vmm.py": "39c03d94eb50df757e9258e3cecf56338a2c9137d0018f4f4a0387309b358f30",
    "snapshot_probe.py": "4ff7bbf0738686a8822e42288043d4fd0a08a800bceec7c38d97022b7643ca3f",
    "vmm_census.py": "78af58d31c0eddd7ed65341c8552cf8e9af4f6cea06ceafe3135d4fce1b1933f",
}


def sha(path):
    with path.open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


def run(a):
    assert re.fullmatch(r"fcmod-zepbound-[a-z0-9-]+", a.unit)
    assert not a.output.exists()
    source = json.loads((a.assets / "source.json").read_bytes())
    review = source["reviewed"]
    assert review["task"] == TASK and (review["vcpus"], review["memory_mib"], review["storage_mib"]) == (1, 2048, 10240)
    trial, = [e for e in review["trials"] if e["trial"].startswith(a.trial + "-")]
    if a.deep:
        assert a.variant in {"v1", "v4"} and trial["trial"] == source["deep_trial"]
    directory = a.assets / "sources" / trial["trial"]
    for name in ("trajectory", "rollout"):
        extension = "json" if name == "trajectory" else "jsonl"
        assert sha(directory / f"{name}.{extension}") == trial[f"{name}_sha256"]
    assert sha(a.binary) == a.binary_sha256
    assert sha(a.codex_tools) == CODEX
    for artifact, digest in TRACE.items():
        assert sha(a.tracer / artifact) == digest
    root = WIDE_ROOT if a.deep else ROOT
    cfg = json.loads((a.runtime / "runtime.json").read_bytes())
    assert cfg["guest_root_mode"] == "disk" and cfg["smt_enabled"] is False
    assert sha(Path(cfg["root_drive"])) == root
    assert bool(cfg.get("root_drive_use_pmem")) == (a.variant == "v4")
    assert bool(cfg.get("free_page_reporting")) == (a.variant == "v2")
    assert bool(cfg.get("guest_reclamation", {}).get("Enabled")) == (a.variant in {"v3", "v4"})
    args = [
        a.tracer / "run-traced.sh", a.binary, a.tracer, a.output,
        "-trajectory", directory / "trajectory.json", "-rollout", directory / "rollout.jsonl",
        "-namespace", "aries", "-socket", f"/run/{a.unit}/containerd.sock",
        "-checkpoint-dir", a.runtime / "checkpoints", "-shim-dir", a.runtime / "shim",
        "-io-dir", f"/run/{a.unit}/aries-io", "-image", review["image"],
        "-codex-tools", a.codex_tools, "-guest-task-v3", "-replacement-openssl",
    ]
    env = dict(os.environ, ZB_VMM_CENSUS="1")
    for key in ("ZB_CHECKPOINT_NAMESPACE_DIR", "ZB_GUEST_VSOCK", "ZB_CONTENT_SAMPLE"):
        env.pop(key, None)
    if a.deep:
        env.update(ZB_GUEST_VSOCK="1", ZB_CONTENT_SAMPLE="8192")
    if a.deep or a.variant in {"v1", "v2"}:
        env["ZB_CHECKPOINT_NAMESPACE_DIR"] = str(a.runtime / "checkpoints" / "aries")
    if a.variant in {"v3", "v4"}:
        args.append("-native-checkpoint")
    result = subprocess.run(args, env=env)
    (a.output / "driver.exit").write_text(f"{result.returncode}\n")
    (a.output / "relaxed-provenance.json").write_text(json.dumps({
        "trial": trial, "variant": a.variant, "node": Path("/var/emulab/boot/nodeid").read_text().strip(),
        "binary_sha256": a.binary_sha256, "argv": [str(arg) for arg in args],
        "root_sha256": root, "measurement": "wide_pfn_deep" if a.deep else "ordinary",
        "scope": "replacement_observed_first_replies",
    }, indent=2) + "\n")
    return result.returncode


if __name__ == "__main__":
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("trial")
    cli.add_argument("variant", choices=("v1", "v2", "v3", "v4"))
    for field in ("binary", "assets", "codex_tools", "tracer", "runtime"):
        cli.add_argument(field, type=lambda p: Path(p).resolve(strict=True))
    cli.add_argument("binary_sha256")
    cli.add_argument("unit")
    cli.add_argument("output", type=lambda p: Path(p).resolve())
    cli.add_argument("--deep", action="store_true")
    sys.exit(run(cli.parse_args()))
