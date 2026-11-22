"""Verify the source-bound token count, wheel bytes and live dataset revisions."""
import ipaddress
import json
import re

from cohort_image_circuit import digest, verify as verify_guest

TASK = "count-dataset-tokens"
CNI_SHA = "8479939145e56bf6480b96f99baeb0b586d039d13ba047e928b692462b0fece0"
ENDPOINTS = {"huggingface.co", "us.aws.cdn.hf.co", "files.pythonhosted.org", "pypi.org"}
REVISIONS = {"dataset": "a82400884621626d41bef89b7604f8054e7e00e0",
             "tokenizer": "989aa7980e4cf806f80c7fef2b1adb7bc71aa306"}


def replay_args(asset, receipt, cni):
    if cni is None:
        raise ValueError("token count requires --dataset-cni with a provisioned private worker bridge")
    cni = cni.resolve(strict=True)
    config = json.loads((cni / "conf/10-video.conflist").read_bytes())
    name = config["name"]
    if not re.fullmatch(r"zbtoken(182|183|189|191|198|201|203)", name):
        raise ValueError("unexpected dataset CNI network")
    if digest(cni / "SHA256SUMS") != CNI_SHA:
        raise ValueError("unexpected CNI plugin digest list")
    for line in (cni / "SHA256SUMS").read_text().splitlines():
        expected, binary = line.split()
        if digest(cni / "bin" / binary) != expected:
            raise ValueError(f"CNI plugin bytes differ: {binary}")
    hosts = json.loads((cni / "conf/dataset-hosts.json").read_bytes())
    if set(hosts) != ENDPOINTS or not all(ipaddress.ip_address(v).is_global for v in hosts.values()):
        raise ValueError("unexpected dataset endpoints")
    args = ["-cni-network", name, "-cni-conf-dir", str(cni / "conf"),
            "-cni-cache-dir", str(cni / "cache"), "-cni-bin-dir", str(cni / "bin")]
    ready, = [e["state"] for e in receipt["source_setup_and_final_state"] if e["event"] == "source_task_ready"]
    if sha := ready.get("debian_curl_archive_sha256"):
        path = asset / "curl-debs.tar.gz"
        if digest(path) != sha:
            raise ValueError("Debian curl archive differs from the live validation")
        args.extend(["-os-tools",str(path)])
    return args


def verify(run, receipt):
    results = verify_guest(run, receipt, sorted_final=True)
    events = [json.loads(line) for line in (run / "events.jsonl").read_bytes().splitlines()]
    nets = [e for e in events if e["event"] == "validated_guest_network"]
    if [e["phase"] for e in nets] != ["before_snapshot", "after_restore"]:
        raise ValueError("missing guest network readback")
    for e in nets:
        net = e["network"]
        if (net["checked_revisions"] != REVISIONS or set(net["resolved_endpoints"]) != ENDPOINTS
                or not all(addresses for addresses in net["resolved_endpoints"].values())):
            raise ValueError("guest endpoints or public revisions differ from the validated source")
    keys = ("step_id", "source_call_id", "rollout_line", "basis")
    external = [tuple(e[k] for k in keys) for e in events if e["event"] == "source_event" and e.get("source_kind")=="web_search_call"]
    if external != [tuple(e[k] for k in keys) for e in receipt["external_completion_events"]]:
        raise ValueError("original external completion boundaries differ")
    final = results["final_checks"].splitlines()
    if "token total 79586" not in final:
        raise ValueError("source aggregate differs")
    wheels = [json.loads(line) for line in final if line.startswith('{"package"')]
    if not wheels or any(not re.fullmatch(r"[0-9a-f]{64}",w["wheel"]["archive_info"]["hashes"]["sha256"]) for w in wheels):
        raise ValueError("missing resolved wheel hash")
    results.update(verified_revisions=REVISIONS, wheel_count=len(wheels), external_completions=len(external))
    return results
