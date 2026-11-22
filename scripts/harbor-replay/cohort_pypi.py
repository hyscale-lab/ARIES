"""Assets, scoped networking and fidelity for reviewed gRPC and financial replays."""
import json
from pathlib import Path
import re

from cohort_image_circuit import IMAGE_HELPER, digest
from cohort_sparql import CNI_SUMS, CODEX_TOOLS, OS_TOOLS


GRPC = "kv-store-grpc"
FINANCIAL = "financial-document-processor"
TASKS = {GRPC, FINANCIAL}
PYTHON_ARCHIVE = "7f8a93c102db3ce25dae68cc485877f8ec9f79edb42a7c4518c96a1153b78682"
IMAGE_HELPER_FILE = "aries-harbor-image"
NETWORK_SUBNETS = {"pypi": 233, "financeapt": 234}


def receipt_path(task, trial):
    return Path("receipts") / f"{task}-{trial}.json"


def pinned(path, sha):
    if not re.fullmatch(r"[0-9a-f]{64}", sha) or digest(path) != sha:
        raise ValueError(f"digest differs for {path}")
    return path


def load_receipt(asset, entry):
    reference = asset / receipt_path(entry["task"], entry["trial_id"])
    pinned(reference, entry["receipt_sha256"])
    receipt = json.loads(reference.read_bytes())
    if any(receipt[key] != entry[key] for key in ("task", "trial_id")):
        raise ValueError("packaged receipt is for another source")
    return receipt


def network_args(cni, profile):
    if cni is None:
        raise ValueError(f"this replay requires a private --{profile}-cni profile")
    cni = cni.resolve(strict=True)
    network = NETWORK_SUBNETS[profile]
    config = json.loads((cni / f"conf/10-{profile}.conflist").read_bytes())
    name = config["name"]
    match = re.fullmatch(fr"zb{profile}([1-9][0-9]{{1,2}})", name)
    if match is None or int(match[1]) > 254 or config["cniVersion"] != "1.0.0":
        raise ValueError(f"unexpected {profile} CNI profile name")
    bridge, firewall, redirect, loopback = config["plugins"]
    ipam = bridge["ipam"]
    if ([p["type"] for p in (bridge, firewall, redirect, loopback)] !=
            ["bridge", "firewall", "tc-redirect-tap", "loopback"] or
            not bridge["ipMasq"] or not bridge["isDefaultGateway"] or bridge["bridge"] != name or
            ipam["subnet"] != f"10.{network}.{match[1]}.0/28" or ipam["type"] != "host-local" or
            Path(ipam["dataDir"]) != cni / "ipam" or Path(ipam["resolvConf"]) != cni / "resolv.conf"):
        raise ValueError("unexpected PyPI CNI scope or plugin configuration")
    for row in pinned(cni / "SHA256SUMS", CNI_SUMS).read_text().splitlines():
        sha, name_on_disk = row.split()
        pinned(cni / "bin" / name_on_disk, sha)
    return ["-cni-network", name, "-cni-conf-dir", str(cni / "conf"),
            "-cni-cache-dir", str(cni / "cache"), "-cni-bin-dir", str(cni / "bin")]


def replay_args(asset, task, cni, profile="pypi"):
    if profile == "financeapt" and task != FINANCIAL:
        raise ValueError("signed Ubuntu snapshots are scoped to their financial trials")
    pinned(asset / "codex-harbor-tools.tar.gz", CODEX_TOOLS)
    args = network_args(cni, profile)
    if task == GRPC:
        return [*args, "-os-tools", str(pinned(asset / "kv-python.tar.gz", PYTHON_ARCHIVE))]
    if task == FINANCIAL:
        return [*args, "-os-tools", str(pinned(asset / f"{task}-debs.tar.gz", OS_TOOLS)),
                "-image-helper", str(pinned(asset / IMAGE_HELPER_FILE, IMAGE_HELPER))]
    raise ValueError(f"unexpected PyPI task {task}")


def events_from(log):
    with Path(log).open() as stream:
        return [json.loads(line) for line in stream]


def boundaries(events):
    dispatch = ("step_id", "rollout_line", "source_call_id", "sequence", "operation")
    inferred = ("step_id", "rollout_line", "source_call_id", "source_kind", "basis", "requested_ns")
    external = {"web_search_call", "harness_rejection_request", "harness_rejection_reply"}
    return {
        "dispatch": [{k: e[k] for k in dispatch} for e in events if e["event"] == "tool_dispatch"],
        "external": [{k: e[k] for k in inferred} for e in events if e["event"] == "source_event" and
                     e.get("source_kind") in external],
    }


def signed_apt_evidence(ready, final, profile):
    snapshot = profile["snapshot"]
    packages = profile["packages"]
    if not re.fullmatch(r"[0-9]{8}T[0-9]{6}Z", snapshot) or not packages:
        raise ValueError("invalid source date or package inventory")
    lines = ready["checks"].splitlines()
    expected = {f"URIs: https://snapshot.ubuntu.com/ubuntu/{snapshot}/",
                "Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg"}
    if not expected <= set(lines) or f"Verified {len(packages)} source-observed APT package versions" not in final.splitlines():
        raise ValueError("signed Ubuntu archive or package-version checks differ")
    return {"snapshot": snapshot, "package_versions": packages}


def verify(log, receipt, reference, network=None, financial_apt=None):
    events = events_from(log)
    phases = ("create_start", "snapshot_start", "snapshot_complete", "restore_start", "restore_complete",
              "session_start", "session_end", "removed")
    if [e["event"] for e in events if e["event"] in phases] != list(phases) or events[-1]["event"] != "removed":
        raise ValueError("PyPI replay lifecycle is incomplete")
    first = events[0]
    fields = ("task", "trial_id", "job_id", "row_id", "image", "trajectory_sha256", "rollout_sha256",
              "memory_mib", "storage_mib", "vcpus")
    if (any(first[k] != receipt[k] for k in fields) or not first["cni_network"] or
            network is not None and first["cni_network"] != network):
        raise ValueError("PyPI replay identity or network differs from its receipt")
    if boundaries(events) != reference:
        raise ValueError("guest dispatch, external completion, or pre-dispatch rejection differs")

    replies = [e for e in events if e["event"] == "tool_reply"]
    calls = {e["sequence"]: e for e in replies}
    expected = {e["sequence"]: e for e in receipt["tools"]}
    if calls.keys() != expected.keys() or len(calls) != len(replies):
        raise ValueError("tool sequence differs from the validated source")
    fields = ("step_id", "rollout_line", "operation", "exit_code", "expected_exit_code")
    images = 0
    for sequence, observed in calls.items():
        source = expected[sequence]
        if any(observed[k] != source[k] for k in fields):
            raise ValueError(f"guest action or source exit mismatch for sequence {sequence}")
        if observed["operation"] == "ViewImage":
            if observed["stdout_sha256"] != source["source_output_sha256"] or observed["stdout_sha256"] != observed["source_output_sha256"]:
                raise ValueError(f"guest attachment differs for sequence {sequence}")
            images += 1
    helpers = [e["guest_sha256"] for e in events if e["event"] == "guest_prepared" and e["helper"] == IMAGE_HELPER_FILE]
    if images and helpers != [IMAGE_HELPER]:
        raise ValueError("guest attachment codec differs from the validated binary")

    connectivity = [e for e in events if e["event"] == "validated_guest_network"]
    if [e["phase"] for e in connectivity] != ["before_snapshot", "after_restore"]:
        raise ValueError("PyPI reachability evidence is incomplete")
    for e in connectivity:
        guest = e["network"]
        if (guest["pypi_status"] != 200 or not guest["pypi_addresses"] or
                not any(len(fields) >= 2 and fields[1] == "00000000" for fields in
                        map(str.split, guest["route"].splitlines()[1:])) or
                not any(line.startswith("nameserver ") for line in guest["resolv_conf"].splitlines())):
            raise ValueError("guest PyPI DNS, route, or TLS evidence differs")
    states = {}
    for event, key in (("source_task_ready", "state"), ("final_task_state", "checks")):
        actual, = [e[key] for e in events if e["event"] == event]
        validated, = [e[key] for e in receipt["source_setup_and_final_state"] if e["event"] == event]
        if actual != validated:
            raise ValueError(f"{event} differs from the native validation")
        states[event] = actual
    checks = {"trial": receipt["trial_id"], "guest_calls": len(calls), "wire_identical_images": images,
            "external_and_rejected_boundaries": reference["external"], "cni_network": first["cni_network"],
            "pypi_http_status": {e["phase"]: e["network"]["pypi_status"] for e in connectivity},
            "final_checks": states["final_task_state"]}
    if financial_apt is not None:
        if receipt["task"] != FINANCIAL or not re.fullmatch(r"zbfinanceapt[1-9][0-9]{1,2}", first["cni_network"]):
            raise ValueError("signed Ubuntu evidence lacks its dedicated network")
        checks["signed_ubuntu_archive"] = signed_apt_evidence(states["source_task_ready"], states["final_task_state"], financial_apt)
    return checks
