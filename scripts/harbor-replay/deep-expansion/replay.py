"""Replay one pinned expansion source with the shared deep-memory probes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import runpy
import subprocess
import sys

CLI = "557158ab56d79ee3f6fca0300f6b0dc423724d4b795adecb5d738a6cb5155541"
REVISION = "c0a6633e0bc726c35cfa447bf47524002d8d0439"
ROOT = "8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5"
TASKS = {
    "c3e7094c": ("code-from-image", "313b853378b43b575ed3ee4412533908ca6e9687ea841d1bc933f7ed29f39bbc"),
    "7cbf99b6": ("count-dataset-tokens", "22b925ccf5b1859c0fae75076497726f451e4dce34673b23a08a13c9c63b092d"),
    "3e740f93": ("financial-document-processor", (
        "46b62165ba0a562983628969d1e40b5ae38d3f65520d852c7c1f0d739a9bf732",
        "43bded7b1ead1786ae8c1ed189433ab465d0d6f3cfbfb053aad3a3db5245f5e1",
    )),
    "ec088b8c": ("sparql-university", "acea92c855e0ee53de2c56bfeaf997ce59780dddbff5b8a1a2c73ad62e54ca9a"),
    "3c332446": ("regex-log", "6ca6c3eb7026ec9ff60bb4628b40df3d0e309e3619602f698c54a5977331421b"),
    "1058f9a0": ("constraints-scheduling", "6ca6c3eb7026ec9ff60bb4628b40df3d0e309e3619602f698c54a5977331421b"),
}


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def verify_calendar(raw, receipt, trajectory):
    """Verify the original calendar checks while retaining its live DTSTAMP."""
    with (raw / "events.jsonl").open() as stream:
        events = [json.loads(line) for line in stream]
    assert events[-1]["event"] == "removed" and any(e["event"] == "session_end" for e in events)
    calls = sorted((e for e in events if e["event"] == "tool_reply"), key=lambda e: e["sequence"])
    fields = ("sequence", "step_id", "rollout_line", "exit_code", "expected_exit_code", "operation", "source_output_sha256")
    assert [tuple(e[k] for k in fields) for e in calls] == [tuple(e[k] for k in fields) for e in receipt["tools"]]
    native = calls[-1]
    assert native["step_id"] == 12 and native["exit_code"] == 0
    assert native.get("stdout", "").rstrip("\n") == receipt["final_tool_stdout"].rstrip("\n")
    assert native.get("stderr", "") == receipt["final_tool_stderr"]
    final, = [e["checks"] for e in events if e["event"] == "final_task_state"]
    expected, = [e["checks"] for e in receipt["source_setup_and_final_state"] if e["event"] == "final_task_state"]
    lines, reference = final.splitlines(), expected.splitlines()
    assert re.fullmatch(r"[0-9a-f]{64}  meeting_scheduled\.ics", lines[0]) and lines[1:] == reference[1:]
    spec, = [s for s in json.loads(trajectory.read_bytes())["steps"] if s["step_id"] == 11]
    command, = [c["arguments"]["cmd"] for c in spec["tool_calls"] if c["function_name"] == "exec_command"]
    assert "timestamp=$(date -u +%Y%m%dT%H%M%SZ)" in command and "DTSTAMP:${timestamp}" in command
    return dict(trial=receipt["trial_id"], guest_calls=len(calls), final_checks=final,
                dynamic_dtstamp_requested=True, source_calendar_assertions_match=True,
                calendar_sha256=lines[0].split()[0], reference_calendar_sha256=reference[0].split()[0])


def profile(assets, trial, cni=None, *, recorded_network=None):
    task, seal = TASKS[trial]
    assert sha(assets / "SHA256SUMS") in ((seal,) if isinstance(seal, str) else seal)
    subprocess.run(["sha256sum", "--quiet", "-c", "SHA256SUMS"], cwd=assets, check=True)
    entry, = [e for e in json.loads((assets / "manifest.json").read_bytes())
              if e["task"] == task and e["trial_id"].startswith(trial + "-")]
    sys.path.insert(0, str(assets))
    if task == "financial-document-processor":
        import cohort_pypi
        receipt = cohort_pypi.load_receipt(assets, entry)
        extra = cohort_pypi.replay_args(assets, task, cni) if cni is not None else []
        network = recorded_network if recorded_network else extra[extra.index("-cni-network") + 1]
        verify = lambda raw: cohort_pypi.verify(raw / "events.jsonl", receipt, entry["reference_boundaries"], network)
    else:
        receipt = entry["receipt"]
        if task == "count-dataset-tokens":
            import cohort_dataset
            extra = cohort_dataset.replay_args(assets, receipt, cni) if cni is not None else []
            verify = lambda raw: cohort_dataset.verify(raw, receipt)
        elif task == "sparql-university":
            import cohort_sparql
            extra = cohort_sparql.replay_args(assets, cni) if cni is not None else []
            verify = lambda raw: cohort_sparql.verify(raw / "events.jsonl", receipt)
        else:
            from cohort_image_circuit import verify as verify_source
            if task == "code-from-image":
                extra = ["-image-helper", str(assets / "aries-harbor-image")]
            else:
                archive = assets / f"{task}-debs.tar.gz"
                ready, = [e["state"] for e in receipt["source_setup_and_final_state"] if e["event"] == "source_task_ready"]
                assert sha(archive) == ready["ubuntu_packages_sha256"]
                extra = ["-os-tools", str(archive)]
            verify = lambda raw: verify_source(raw, receipt)
    assert receipt["session_completed"] and receipt["all_observed_tool_exits_match"]
    assert not any(receipt.get(k) for k in ("diagnostic_process_drift", "diagnostic_video_images"))
    assert (receipt["memory_mib"], receipt["storage_mib"], receipt["vcpus"]) == (
        4096 if task == "financial-document-processor" else 2048, 10240, 1,
    )
    directory = assets / "sources" / task / receipt["trial_id"]
    for filename, key in (("rollout.jsonl", "rollout_sha256"), ("trajectory.json", "trajectory_sha256")):
        assert sha(directory / filename) == receipt[key]
    if task == "constraints-scheduling":
        verify = lambda raw: verify_calendar(raw, receipt, directory / "trajectory.json")
    return receipt, extra, verify


def run(a):
    assert sha(a.cli) == CLI and not a.output.exists()
    pin = runpy.run_path(str(a.trace_contract))["TRACE"]
    assert all(sha(a.tracer / name) == value for name, value in pin.items())
    cfg = json.loads((a.runtime / "runtime.json").read_bytes())
    assert cfg["guest_root_mode"] == "disk" and cfg["smt_enabled"] is False
    assert sha(Path(cfg["root_drive"])) == ROOT
    assert bool(cfg.get("root_drive_use_pmem")) == (a.variant == "v4")
    assert bool(cfg.get("guest_reclamation", {}).get("Enabled")) == (a.variant == "v4")
    assert not cfg.get("free_page_reporting")
    assert a.variant == "v4" or "damon_reclaim.enabled=N" in cfg["kernel_args"].split()
    receipt, extra, verify = profile(a.assets, a.trial, a.cni)
    assert receipt["task"] in {"code-from-image", "regex-log", "constraints-scheduling"} or "-cni-network" in extra
    source = a.assets / "sources" / receipt["task"] / receipt["trial_id"]
    args = [
        a.tracer / "run-traced.sh", a.cli, a.tracer, a.output,
        "-trajectory", source / "trajectory.json", "-rollout", source / "rollout.jsonl",
        "-namespace", "aries", "-socket", f"/run/{a.unit}/containerd.sock",
        "-checkpoint-dir", a.runtime / "checkpoints", "-shim-dir", a.runtime / "shim",
        "-io-dir", f"/run/{a.unit}/aries-io", "-image", receipt["image"],
        "-codex-tools", a.assets / "codex-harbor-tools.tar.gz", "-guest-task-v3", *extra,
    ]
    if a.variant == "v4":
        args.append("-native-checkpoint")
    env = dict(os.environ, ZB_GUEST_VSOCK="1", ZB_CONTENT_SAMPLE="8192", ZB_VMM_CENSUS="1",
               ZB_CHECKPOINT_NAMESPACE_DIR=str(a.runtime / "checkpoints" / "aries"))
    rc = subprocess.run(args, env=env).returncode
    (a.output / "driver.exit").write_text(f"{rc}\n")
    (a.output / "expansion-provenance.json").write_text(json.dumps({
        "argv": [str(arg) for arg in args], "adapter_binary_sha256": CLI, "adapter_revision": REVISION,
        "source_inventory_sha256": sha(a.assets / "SHA256SUMS"), "trace_protocol": "wide-pfn-8192",
    }, indent=2) + "\n")
    if rc == 0:
        try:
            (a.output / "fidelity-checks.json").write_text(json.dumps(verify(a.output), indent=2) + "\n")
        except Exception:
            (a.output / "driver.exit").write_text("1\n")
            raise
    return rc


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("trial", choices=TASKS)
    parser.add_argument("variant", choices=("v1", "v4"))
    for name in ("cli", "assets", "tracer", "trace_contract", "runtime"):
        parser.add_argument(name, type=lambda p: Path(p).resolve(strict=True))
    parser.add_argument("unit")
    parser.add_argument("output", type=lambda p: Path(p).resolve())
    parser.add_argument("--cni", type=lambda p: Path(p).resolve(strict=True))
    sys.exit(run(parser.parse_args()))
