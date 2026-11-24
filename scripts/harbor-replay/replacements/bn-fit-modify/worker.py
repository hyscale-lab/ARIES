"""Replay an exact source in a qualified native runtime, then preserve provenance."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


ROOTS = {
    "ordinary": "292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f",
    "deep": "8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5",
}
CODEX = "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab"


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def run(a):
    assert not a.output.exists()
    config = json.loads((a.stage / "source.json").read_bytes())
    profile = config["reviewed"]
    assert profile["task"] == "bn-fit-modify" and not profile["needs_network"]
    assert (profile["vcpus"], profile["memory_mib"], profile["storage_mib"]) == (1, 2048, 10240)
    trial, = [r for r in profile["trials"] if r["trial"].startswith(a.trial + "-")]
    if a.mode == "deep":
        assert a.variant in {"v1", "v4"} and trial["trial"] == config["median_source"]
    source = a.stage / "sources" / trial["trial"]
    for name, field in (("trajectory.json", "trajectory_sha256"), ("rollout.jsonl", "rollout_sha256")):
        assert sha(source / name) == trial[field]
    tracer = a.stage / "tracer"
    assert all(sha(tracer / name) == digest for name, digest in config["trace_sha256"].items())
    binary = a.stage / "aries-bn-fit-modify"
    adapter = sha(binary)
    assert adapter == (a.stage / "adapter.sha256").read_text().strip() and sha(a.codex_tools) == CODEX
    cfg = json.loads((a.runtime / "runtime.json").read_bytes())
    root = ROOTS[a.mode]
    assert cfg["guest_root_mode"] == "disk" and cfg["smt_enabled"] is False and sha(Path(cfg["root_drive"])) == root
    assert bool(cfg.get("root_drive_use_pmem")) == (a.variant == "v4")
    assert bool(cfg.get("free_page_reporting")) == (a.variant == "v2")
    assert bool(cfg.get("guest_reclamation", {}).get("Enabled")) == (a.variant in {"v3", "v4"})
    args = [tracer / "run-traced.sh", binary, tracer, a.output,
            "-trajectory", source / "trajectory.json", "-rollout", source / "rollout.jsonl",
            "-namespace", "aries", "-socket", f"/run/{a.unit}/containerd.sock",
            "-checkpoint-dir", a.runtime / "checkpoints", "-shim-dir", a.runtime / "shim",
            "-io-dir", f"/run/{a.unit}/aries-io", "-image", profile["image"],
            "-codex-tools", a.codex_tools, "-guest-task-v3", "-replacement-bn-fit"]
    env = dict(os.environ, ZB_VMM_CENSUS="1", BPFTRACE_PERF_RB_PAGES="1024")
    for key in ("ZB_CHECKPOINT_NAMESPACE_DIR", "ZB_GUEST_VSOCK", "ZB_CONTENT_SAMPLE"):
        env.pop(key, None)
    if a.mode == "deep":
        env.update(ZB_GUEST_VSOCK="1", ZB_CONTENT_SAMPLE="8192")
    if a.mode == "deep" or a.variant in {"v1", "v2"}:
        env["ZB_CHECKPOINT_NAMESPACE_DIR"] = str(a.runtime / "checkpoints/aries")
    if a.variant in {"v3", "v4"}:
        args.append("-native-checkpoint")
    result = subprocess.run(args, env=env)
    (a.output / "driver.exit").write_text(f"{result.returncode}\n")
    (a.output / "relaxed-provenance.json").write_text(json.dumps(dict(
        trial=trial, variant=a.variant, node=Path("/var/emulab/boot/nodeid").read_text().strip(),
        binary_sha256=adapter, argv=list(map(str, args)), root_sha256=root, input= config["input"],
        measurement="wide_pfn_deep" if a.mode == "deep" else "ordinary", scope="replacement_observed_first_replies",
    ), indent=2) + "\n")
    return result.returncode


if __name__ == "__main__":
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("trial")
    cli.add_argument("variant", choices=("v1", "v2", "v3", "v4"))
    cli.add_argument("mode", choices=ROOTS)
    for field in ("stage", "runtime", "codex_tools"):
        cli.add_argument(field, type=lambda p: Path(p).resolve(strict=True))
    cli.add_argument("unit")
    cli.add_argument("output", type=lambda p: Path(p).resolve())
    sys.exit(run(cli.parse_args()))
