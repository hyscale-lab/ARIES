"""Audit source-bound inference wheels, model bytes and scoped network evidence."""
import ipaddress
import json
from pathlib import Path
import re
import tarfile

from cohort_pypi import pinned
from cohort_sparql import CNI_SUMS, CODEX_TOOLS


TASK = "hf-model-inference"
ENVIRONMENT = "hf-environment.tar.gz"
ENVIRONMENT_SHA = "04ac35b54101efa80a45737dac838f3e51dfbd481f079be1495ffdfe4f05732a"
MODEL_REVISION = "714eb0fa89d2f80546fda750413ed43d93601a13"
MODEL_SHA = "7c3919835e442510166d267fe7cbe847e0c51cd26d9ba07b89a57b952b49b8aa"
ENDPOINTS = {"huggingface.co", "us.aws.cdn.hf.co", "cas-server.xethub.hf.co",
             "transfer.xethub.hf.co", "pypi.org", "files.pythonhosted.org"}


def normalize(package):
    return re.sub(r"[-_.]+", "-", package).lower()


def source_environment(path):
    with tarfile.open(pinned(path, ENVIRONMENT_SHA), "r:gz") as archive:
        member = archive.extractfile("tmp/aries-hf-setup/wheels.json")
        if member is None:
            raise ValueError("inference wheel inventory is missing")
        manifest = json.load(member)
    wheels = {normalize(w["name"]): {k: w[k] for k in ("version", "sha256")} for w in manifest["wheels"]}
    versions = {name: w["version"] for name, w in wheels.items()}
    versions.update(manifest["image_package"])
    return {"initial_package_versions": versions, "initial_wheel_inventory": wheels}


def replay_args(asset, cni):
    if cni is None:
        raise ValueError("inference requires --hf-cni with a provisioned private worker profile")
    cni = cni.resolve(strict=True)
    config = json.loads((cni / "conf/10-video.conflist").read_bytes())
    name = config["name"]
    match = re.fullmatch(r"zbhfm(182|183|189|191|198|200|201|203)", name)
    if match is None:
        raise ValueError("unexpected measured inference CNI profile")
    subnet_octet = 237 if name == "zbhfm189" else 235
    bridge, firewall, redirect, loopback = config["plugins"]
    if ([p["type"] for p in (bridge, firewall, redirect, loopback)] !=
            ["bridge", "firewall", "tc-redirect-tap", "loopback"] or bridge["bridge"] != name or
            bridge["ipam"]["subnet"] != f"10.{subnet_octet}.{match[1]}.0/28" or
            Path(bridge["ipam"]["dataDir"]) != cni / "ipam" or
            Path(bridge["ipam"]["resolvConf"]) != cni / "resolv.conf"):
        raise ValueError("inference subnet, CNI plugin or resolver path differs")
    host_map = json.loads((cni / "conf/dataset-hosts.json").read_bytes())
    if set(host_map) != ENDPOINTS or not all(ipaddress.ip_address(addr).is_global for addr in host_map.values()):
        raise ValueError("unexpected inference endpoints")
    if (cni / "resolv.conf").read_text().split() != ["nameserver", f"10.{subnet_octet}.{match[1]}.1"]:
        raise ValueError("inference must use the private DNS service")
    for line in pinned(cni / "SHA256SUMS", CNI_SUMS).read_text().splitlines():
        sha, binary = line.split()
        pinned(cni / "bin" / binary, sha)
    pinned(asset / "codex-harbor-tools.tar.gz", CODEX_TOOLS)
    return ["-cni-network", name, "-cni-conf-dir", str(cni / "conf"), "-cni-cache-dir", str(cni / "cache"),
            "-cni-bin-dir", str(cni / "bin"), "-os-tools", str(pinned(asset / ENVIRONMENT, ENVIRONMENT_SHA))]


def package_versions(ready):
    if ready["inference_environment_sha256"] != ENVIRONMENT_SHA or ready["codex_prefix_sha256"] != CODEX_TOOLS:
        raise ValueError("inference ready-state archive digest differs")
    versions, = [json.loads(line) for line in ready["checks"].splitlines() if line.startswith('{"blinker":')]
    return versions


def evidence(ready, final, connectivity, profile):
    versions = package_versions(ready)
    if versions != profile["initial_package_versions"]:
        raise ValueError("installed inference dependency versions differ")
    if any(e["network"] != {"pypi_status": 200, "model_revision": MODEL_REVISION} for e in connectivity):
        raise ValueError("live inference dependency endpoint or model revision differs")
    files = profile["files"]
    lines = set(final.splitlines())
    expected = {f"{file}: OK" for file in files}
    expected.update({f"{files['app.py']}  app.py", "model_cache/sentiment_model/model.safetensors: OK",
                     "Guest sentiment service returned positive"})
    if not expected <= lines:
        raise ValueError("inference source artifacts, model or live service check differs")
    return {"initial_package_versions": versions, "initial_environment_sha256": ENVIRONMENT_SHA,
            "downloaded_model": {"revision": MODEL_REVISION, "sha256": MODEL_SHA},
            "source_artifact_sha256": files, "source_server_dependency": profile["dependency"],
            "live_sentiment": "positive"}


def _assertion_lines(output):
    """Keep every printed HTTP assertion, including response shape and predicted label."""
    import ast
    import math
    rows, probabilities = [], []
    for line in output.splitlines():
        if not (line.startswith("PASS ") or re.fullmatch(r"All \d+ .+ checks passed\.|All \d+ checks passed\.|\d+ HTTP checks passed in total; background service remains running\.", line)):
            continue
        if "confidence" in line:
            prefix, _, payload = line.partition("{")
            response = ast.literal_eval("{" + payload)
            scores = response["confidence"]
            if (set(response) != {"sentiment", "confidence"} or set(scores) != {"negative", "positive"}
                    or any(not math.isfinite(v) or not 0 <= v <= 1 for v in scores.values())
                    or abs(sum(scores.values()) - 1) > 1e-5 or max(scores, key=scores.get) != response["sentiment"]):
                raise ValueError("original HTTP assertion returned an invalid confidence or label")
            probabilities.append(scores["positive"])
            line = prefix + response["sentiment"]
        rows.append(re.sub(r"\bPID \d+", "PID <pid>", line))
    return rows, probabilities


def assertion_evidence(events, trial):
    """Verify original source assertion outputs across native exec/start/poll boundaries."""
    import hashlib
    checks = []
    replies = {e["source_call_id"]: e for e in events if e["event"] == "tool_reply"}
    for group in _HTTP_ASSERTIONS[trial]:
        calls = [replies[c] for c in group["calls"]]
        if [c["exit_code"] for c in calls] != group["exits"] or any(c["exit_code"] != c["expected_exit_code"] for c in calls):
            raise ValueError(f"{trial} assertion process or poll status differs")
        if len(calls) > 1 and (not calls[0].get("source_process_session_id") or
                               len({c.get("source_process_session_id") for c in calls}) != 1):
            raise ValueError(f"{trial} assertion process handle differs")
        if any("Traceback (most recent call last):" in (c.get("stderr", "") + c.get("stdout", "")) for c in calls):
            raise ValueError(f"{trial} assertion subprocess printed a traceback")
        rows, scores = _assertion_lines("".join(c.get("stdout", "") for c in calls))
        digest = hashlib.sha256("\n".join(rows).encode()).hexdigest()
        if digest != group["sha256"] or len(scores) != len(group["positive"]):
            raise ValueError(f"{trial} original HTTP assertion lines or completion marker differ")
        delta = max((abs(a-b) for a, b in zip(scores, group["positive"])), default=0.0)
        if delta > 2e-6:
            raise ValueError(f"{trial} HTTP prediction probabilities differ by {delta}")
        checks.append({"source_call_ids": group["calls"], "assertion_lines": len(rows), "completion": rows[-1],
                       "assertion_sha256": digest, "max_positive_score_difference": delta})
    return checks


# Normalized original assertion lines, call IDs, status and positive scores from the reviewed ATIF.
_HTTP_ASSERTIONS = {'071ce48d-3de4-4c02-9e42-3eb64933acd2': [{'calls': ['call_RVX7Yd76NSEQfxM6T6783Cqa'],
                                           'exits': [0],
                                           'sha256': 'c8578a5d67e74db494f3c5a78ce232ae458c14986c3d8959ae7765026690fe52',
                                           'positive': [0.9998770952224731,
                                                        0.00034663110272958875,
                                                        0.9998759031295776,
                                                        0.9998331069946289]}],
 '249f2934-972f-4c61-af3d-0315ce5003c0': [{'calls': ['call_BXiZPJzgIedKxjmoavNfGVSC',
                                                     'call_0haVTSFTqCOZeo8LhB7wryCF'],
                                           'exits': [-1, 0],
                                           'sha256': '614480a3d441a4c9adaa94a5c65890a56b78d0d8a2f5a6483a586eddddff2fe9',
                                           'positive': [0.9998781681060791,
                                                        0.0004967869026586413,
                                                        0.9998834133148193,
                                                        0.9998430013656616]},
                                          {'calls': ['call_9y3NHVgP4f8nyfbRIa3Mafh1'],
                                           'exits': [0],
                                           'sha256': '3f457e4fea33a2daf17eac045ff8c7e322a4a9c71703b2ea113c59833450aaf4',
                                           'positive': [0.9998841285705566]}],
 '85e3b0db-f1cb-43b5-9f8a-a2333db196c1': [{'calls': ['call_ssgTQvpWFIidp7Gtcm7c2MOW',
                                                     'call_INKnZqY4Lvr9jdUsqjxcNviL'],
                                           'exits': [-1, 0],
                                           'sha256': '4b94c2a310d15e5367adaffea7352894a609254366f064f7da2737b1bc8d64f7',
                                           'positive': [0.999871015548706,
                                                        0.0003970198449678719,
                                                        0.9998226761817932,
                                                        0.9998167157173157]}],
 'b615a3ec-fae8-4198-ae4c-0aedf9c3e25b': [{'calls': ['call_TOVrT7o3WvteIU8VjuOMc5xs'],
                                           'exits': [0],
                                           'sha256': '8305d942f01715b08c9a60b579d13449eae6d123ac7002d6ee10e7bb73963c08',
                                           'positive': [0.9998739957809448,
                                                        0.0005126027390360832,
                                                        0.9998776912689209,
                                                        0.9998797178268433,
                                                        0.03048323281109333,
                                                        0.9998064637184143]}],
 'f0d5e4b5-b20d-40bf-89ec-6483b65525dc': [{'calls': ['call_CiO9Zr48IxjvxuimjMzwAXrP',
                                                     'call_8y4M5J4CQ3u8bqoXfo63OA1U'],
                                           'exits': [-1, 0],
                                           'sha256': '5c91fdac1164de1d11bd695e7de703b05639fdef12b94ce04e7ba01a7a0c090e',
                                           'positive': [0.9998718500137329,
                                                        0.00018933502724394202,
                                                        0.999824583530426,
                                                        0.999873161315918]}]}
