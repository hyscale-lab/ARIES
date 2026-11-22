"""Asset and fidelity checks for measured SPARQL replays."""
import json
from pathlib import Path
import re

from cohort_image_circuit import digest


TASK = "sparql-university"
OS_TOOLS = "53128b1ff531fb50b8952d571e0af2b5a73b143ae06531f729b172b38e2ced4d"
CODEX_TOOLS = "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab"
CNI_SUMS = "8479939145e56bf6480b96f99baeb0b586d039d13ba047e928b692462b0fece0"
TLS_PROBES = {"bootstrap.pypa.io": 200, "pypi.org": 200}


def network_args(cni):
    if cni is None:
        raise ValueError("SPARQL requires --sparql-cni with a provisioned private worker profile")
    cni = cni.resolve(strict=True)
    config = json.loads((cni / "conf/10-sparql.conflist").read_text())
    name = config["name"]
    if not re.fullmatch(r"zbsparql[1-9][0-9]{1,2}", name):
        raise ValueError("unexpected SPARQL CNI profile")
    if digest(cni / "SHA256SUMS") != CNI_SUMS:
        raise ValueError("CNI digest list differs from the source validation")
    for row in (cni / "SHA256SUMS").read_text().splitlines():
        sha, binary = row.split()
        if digest(cni / "bin" / binary) != sha:
            raise ValueError(f"incorrect {binary} CNI bytes")
    return ["-cni-network", name, "-cni-conf-dir", str(cni / "conf"),
            "-cni-cache-dir", str(cni / "cache"), "-cni-bin-dir", str(cni / "bin")]


def replay_args(asset, cni):
    path = asset / (TASK + "-debs.tar.gz")
    if digest(path) != OS_TOOLS or digest(asset / "codex-harbor-tools.tar.gz") != CODEX_TOOLS:
        raise ValueError("incorrect SPARQL guest tool archive")
    return ["-os-tools", str(path), *network_args(cni)]


def verify(log, receipt):
    """Check guest call identities, external boundaries, dependency downloads and final bytes."""
    events = [json.loads(line) for line in Path(log).read_bytes().splitlines()]
    source = receipt["replay"]
    if not events or events[-1]["event"] != "removed" or not any(e["event"] == "session_end" for e in events):
        raise ValueError("incomplete SPARQL replay")
    for key in ("task", "trial_id", "job_id", "row_id", "image", "trajectory_sha256", "rollout_sha256", "memory_mib", "storage_mib", "vcpus"):
        if events[0][key] != receipt[key]:
            raise ValueError(f"SPARQL identity mismatch for {key}")
    replies = [e for e in events if e["event"] == "tool_reply"]
    keys = ("step_id", "sequence", "rollout_line", "source_call_id", "operation", "exit_code", "expected_exit_code")
    for actual, expected in zip(replies, source["tools"], strict=True):
        if any(actual[k] != expected[k] for k in keys):
            raise ValueError("SPARQL guest call identity or process status differs")
    external = [e for e in events if e["event"] == "source_event" and e.get("source_kind") == "web_search_call"]
    for actual, expected in zip(external, receipt["external_actions"], strict=True):
        if ((actual["step_id"], actual["rollout_line"], actual["source_call_id"], actual["basis"])
                != (expected["step_id"], expected["rollout_line"], expected["call_id"], expected["wait_basis"])):
            raise ValueError("SPARQL external completion differs from original")
    network = {e["phase"]: {host: value["status"] for host, value in e["network"]["dependencies"].items()}
               for e in events if e["event"] == "validated_guest_network"}
    if network != {phase: TLS_PROBES for phase in ("before_snapshot", "after_restore")}:
        raise ValueError("SPARQL dependency network checks missing")
    final, = [e["checks"] for e in events if e["event"] == "final_task_state"]
    if sorted(final.splitlines()) != sorted(source["guest_final_state"]["checks"].splitlines()):
        raise ValueError("SPARQL output, graph or downloaded dependency bytes changed")
    if replies[-1].get("stdout", "").rstrip("\n") != source["final_tool_stdout"].rstrip("\n"):
        raise ValueError("SPARQL final validation output changed")
    return {"trial": receipt["trial_id"], "guest_calls": len(replies), "external_completion_boundaries": len(external),
            "dependency_tls_http_status": network, "final_checks": final}
