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


def network_args(cni, network_prefix):
    if cni is None:
        raise ValueError("a provisioned private worker bridge is required")
    cni = cni.resolve(strict=True)
    config = json.loads((cni / "conf/10-video.conflist").read_bytes())
    name = config["name"]
    if not re.fullmatch(network_prefix + r"(182|183|189|191|198|200|201|203)", name):
        raise ValueError("unexpected worker CNI network")
    if digest(cni / "SHA256SUMS") != CNI_SHA:
        raise ValueError("unexpected CNI plugin digest list")
    for line in (cni / "SHA256SUMS").read_text().splitlines():
        expected, binary = line.split()
        if digest(cni / "bin" / binary) != expected:
            raise ValueError(f"CNI plugin bytes differ: {binary}")
    return ["-cni-network", name, "-cni-conf-dir", str(cni / "conf"),
            "-cni-cache-dir", str(cni / "cache"), "-cni-bin-dir", str(cni / "bin")]


def replay_args(asset, receipt, cni):
    args = network_args(cni, "zbtoken")
    hosts = json.loads((cni / "conf/dataset-hosts.json").read_bytes())
    if set(hosts) != ENDPOINTS or not all(ipaddress.ip_address(v).is_global for v in hosts.values()):
        raise ValueError("unexpected dataset endpoints")
    ready, = [e["state"] for e in receipt["source_setup_and_final_state"] if e["event"] == "source_task_ready"]
    if sha := ready.get("python_environment_sha256"):
        python = asset / "kv-python.tar.gz"
        curl = asset / "curl-debs.tar.gz"
        if (sha != "7f8a93c102db3ce25dae68cc485877f8ec9f79edb42a7c4518c96a1153b78682"
                or digest(python) != sha or digest(curl) != ready["debian_curl_archive_sha256"]):
            raise ValueError("source Python or curl archive differs from the native validation")
        return [*args, "-os-tools", str(python), "-curl-packages", str(curl)]
    for field, name in (("debian_curl_archive_sha256","curl-debs.tar.gz"),
                        ("debian_patch_archive_sha256","circuit-patch-debs.tar.gz")):
        if sha := ready.get(field):
            path = asset / name
            if digest(path) != sha:
                raise ValueError(f"{name} differs from the live validation")
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
