"""Execute a single missing repository sanitation cell on an isolated worker."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

TASK = "sanitize-git-repo"
MEDIAN = "6d0403e0-740e-4104-a684-619cfc8f85bf"
ROOT = "292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f"
WIDE_ROOT = "8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5"
CODEX = "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab"
CURL = "6e8e1dac4bc58e974f2654b53c6e4d5210cc8b7f492f1ad5660cdab5ac4c6998"
IMAGE_HELPER = "b06faaf4f96c04e41d6808ee8e38317ea1fd19c68b20693b2045c8fe7e712bc9"
STOCK_FC = "b33f46cd6d88834cf6a4f8d184f08f219809c636236bffd286d233a60602a2ba"


def sha(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def launch(a):
    assert not a.output.exists()
    profile = json.loads((a.stage / "retry-source.json").read_bytes())
    trial, = [r for r in profile["trials"] if r["trial"].startswith(f"{a.short_trial}-")]
    assert profile["task"] == TASK and (profile["vcpus"], profile["memory_mib"], profile["storage_mib"]) == (1, 2048, 10240)
    source = a.stage / "sources" / trial["trial"]
    assert len(trial["trial"]) == 36
    for ext, key in (("trajectory.json", "trajectory_sha256"), ("rollout.jsonl", "rollout_sha256")):
        assert sha(source / ext) == trial[key]
    if a.deep:
        assert a.variant in {"v1", "v4"} and trial["trial"] == MEDIAN
    root = WIDE_ROOT if a.deep else ROOT
    config = json.loads((a.runtime / "runtime.json").read_bytes())
    assert config["smt_enabled"] is False and config["guest_root_mode"] == "disk"
    assert sha(Path(config["root_drive"])) == root
    assert sha(a.runtime / "bin/firecracker") == STOCK_FC
    assert bool(config.get("root_drive_use_pmem")) == (a.variant == "v4")
    assert bool(config.get("free_page_reporting")) == (a.variant == "v2")
    assert bool(config.get("guest_reclamation", {}).get("Enabled")) == (a.variant in {"v3", "v4"})
    assert sha(a.codex_tools) == CODEX
    assert sha(a.old_stage / "curl-debs.tar.gz") == CURL
    image_helper = a.old_stage / "aries-harbor-image"
    assert sha(image_helper) == IMAGE_HELPER
    trace = json.loads((a.stage / "trace-digests.json").read_bytes())
    assert trace and all(sha(a.stage / "tracer" / name) == expected for name, expected in trace.items())
    binary = a.stage / "aries-sanitize-retry"
    assert sha(binary) == (a.stage / "adapter.sha256").read_text().strip()
    node = Path("/var/emulab/boot/nodeid").read_text().strip()
    assert node in {"amd191", "amd200", "amd203"}
    network = a.old_stage / "net"
    config = json.loads((network / "conf/10-cleanse.conflist").read_bytes())
    assert config["name"] == f"zbcleanse{node.removeprefix('amd')}"
    with (a.stage / "network.sh").open("rb") as digest_source:
        network_sha = hashlib.file_digest(digest_source, "sha256").hexdigest()
    args = [
        a.stage / "tracer/run-traced.sh", binary, a.stage / "tracer", a.output,
        "-trajectory", source / "trajectory.json", "-rollout", source / "rollout.jsonl",
        "-namespace", "aries", "-socket", f"/run/{a.unit}/containerd.sock",
        "-checkpoint-dir", a.runtime / "checkpoints", "-shim-dir", a.runtime / "shim",
        "-io-dir", f"/run/{a.unit}/aries-io", "-image", profile["image"],
        "-codex-tools", a.codex_tools, "-curl-packages", a.old_stage / "curl-debs.tar.gz",
        "-guest-task-v3", "-relaxed-sanitize-git-repo",
        "-cni-network", config["name"], "-cni-conf-dir", network / "conf",
        "-cni-cache-dir", network / "cache", "-cni-bin-dir", network / "bin",
    ]
    if trial["trial"] == MEDIAN:
        args.extend(["-image-helper", image_helper])
    env = dict(os.environ, ZB_VMM_CENSUS="1")
    for key in ("ZB_CHECKPOINT_NAMESPACE_DIR", "ZB_GUEST_VSOCK", "ZB_CONTENT_SAMPLE"):
        env.pop(key, None)
    if a.deep:
        env.update(ZB_GUEST_VSOCK="1", ZB_CONTENT_SAMPLE="8192")
    if a.deep or a.variant in {"v1", "v2"}:
        env["ZB_CHECKPOINT_NAMESPACE_DIR"] = str(a.runtime / "checkpoints/aries")
    if a.variant in {"v3", "v4"}:
        args.append("-native-checkpoint")
    process = subprocess.run(args, env=env)
    (a.output / "driver.exit").write_text(f"{process.returncode}\n")
    (a.output / "relaxed-provenance.json").write_text(json.dumps({
        "trial": trial, "variant": a.variant, "node": node, "binary_sha256": sha(binary),
        "argv": [str(arg) for arg in args], "root_sha256": root, "network_setup_sha256": network_sha,
        "measurement": "wide_pfn_deep" if a.deep else "ordinary",
        "scope": "replacement_observed_first_replies",
    }, indent=2) + "\n")
    return process.returncode


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("short_trial")
    parser.add_argument("variant", choices=("v1", "v2", "v3", "v4"))
    for field in ("stage", "old_stage", "runtime", "codex_tools"):
        parser.add_argument(field, type=lambda p: Path(p).resolve(strict=True))
    parser.add_argument("unit")
    parser.add_argument("output", type=lambda p: Path(p).resolve())
    parser.add_argument("--deep", action="store_true")
    sys.exit(launch(parser.parse_args()))
