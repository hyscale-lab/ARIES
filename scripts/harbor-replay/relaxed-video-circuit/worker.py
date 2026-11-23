"""Run the five pinned original sources with ordinary traces and observed replies."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


VIDEO = {"020f911b", "361ad8a6", "80f95afc", "aa067203"}
CIRCUIT = "99c36c5d"
ROOT = "292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f"
CODEX = "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab"
IMAGE_HELPER = "b06faaf4f96c04e41d6808ee8e38317ea1fd19c68b20693b2045c8fe7e712bc9"
PATCH = "2d1665bc3a472a5e895657f41612ecf4e40e4349757d6481a19420b57b8146d0"


def sha(path):
    with path.open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


def run(a):
    assert re.fullmatch(r"fcmod-zepbound-[a-z0-9-]+", a.unit)
    assert not a.output.exists()
    cfg = json.loads((a.runtime / "runtime.json").read_bytes())
    assert cfg["guest_root_mode"] == "disk" and cfg["smt_enabled"] is False
    assert sha(Path(cfg["root_drive"])) == ROOT
    assert bool(cfg.get("root_drive_use_pmem")) == (a.variant == "v4")
    assert bool(cfg.get("free_page_reporting")) == (a.variant == "v2")
    assert bool(cfg.get("guest_reclamation", {}).get("Enabled")) == (a.variant in {"v3", "v4"})
    assert sha(a.binary) == a.binary_sha256
    assert sha(a.fixtures / "codex-harbor-tools.tar.gz") == CODEX
    assert sha(a.fixtures / "circuit-patch-debs.tar.gz") == PATCH
    task = "video-processing" if a.trial in VIDEO else "circuit-fibsqrt"
    review, = [e for e in json.loads((a.assets / "reviewed.json").read_bytes()) if e["task"] == task]
    trial, = [e for e in review["trials"] if e["trial"].startswith(a.trial + "-")]
    assert (review["vcpus"], review["memory_mib"], review["storage_mib"]) == (1, 2048, 10240)
    directory = a.assets / "sources" / task / trial["trial"]
    assert sha(directory / "trajectory.json") == trial["trajectory_sha256"]
    assert sha(directory / "rollout.jsonl") == trial["rollout_sha256"]
    args = [
        a.tracer / "run-traced.sh", a.binary, a.tracer, a.output,
        "-trajectory", directory / "trajectory.json", "-rollout", directory / "rollout.jsonl",
        "-namespace", "aries", "-socket", f"/run/{a.unit}/containerd.sock",
        "-checkpoint-dir", a.runtime / "checkpoints", "-shim-dir", a.runtime / "shim",
        "-io-dir", f"/run/{a.unit}/aries-io", "-image", review["image"],
        "-codex-tools", a.fixtures / "codex-harbor-tools.tar.gz", "-guest-task-v3",
        "-relaxed-video-circuit",
    ]
    if a.trial in VIDEO:
        from cohort_dataset import network_args
        assert sha(a.fixtures / "aries-harbor-image") == IMAGE_HELPER
        args.extend(["-image-helper", a.fixtures / "aries-harbor-image", *network_args(a.cni, "zbvideo")])
    if a.trial in {CIRCUIT, "80f95afc", "020f911b"}:
        args.extend(["-os-tools", a.fixtures / "circuit-patch-debs.tar.gz"])
    env = dict(os.environ, ZB_VMM_CENSUS="1")
    if a.variant in {"v1", "v2"}:
        env["ZB_CHECKPOINT_NAMESPACE_DIR"] = str(a.runtime / "checkpoints" / "aries")
    else:
        env.pop("ZB_CHECKPOINT_NAMESPACE_DIR", None)
        args.append("-native-checkpoint")
    result = subprocess.run(args, env=env)
    (a.output / "driver.exit").write_text(f"{result.returncode}\n")
    (a.output / "relaxed-provenance.json").write_text(json.dumps({
        "trial": trial, "variant": a.variant, "node": Path("/var/emulab/boot/nodeid").read_text().strip(),
        "binary_sha256": a.binary_sha256, "argv": [str(arg) for arg in args],
        "root_sha256": ROOT, "scope": "alternate_fidelity_actual_recorded_replies",
    }, indent=2) + "\n")
    return result.returncode


if __name__ == "__main__":
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("trial", choices=sorted(VIDEO | {CIRCUIT}))
    cli.add_argument("variant", choices=("v1", "v2", "v3", "v4"))
    for field in ("binary", "assets", "fixtures", "tracer", "runtime"):
        cli.add_argument(field, type=lambda p: Path(p).resolve(strict=True))
    cli.add_argument("binary_sha256")
    cli.add_argument("unit")
    cli.add_argument("output", type=lambda p: Path(p).resolve())
    cli.add_argument("--cni", type=lambda p: Path(p).resolve(strict=True))
    sys.exit(run(cli.parse_args()))
