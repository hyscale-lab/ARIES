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
    bridge, firewall, redirect, loopback = config["plugins"]
    if ([p["type"] for p in (bridge, firewall, redirect, loopback)] !=
            ["bridge", "firewall", "tc-redirect-tap", "loopback"] or bridge["bridge"] != name or
            bridge["ipam"]["subnet"] != f"10.235.{match[1]}.0/28" or
            Path(bridge["ipam"]["dataDir"]) != cni / "ipam" or
            Path(bridge["ipam"]["resolvConf"]) != cni / "resolv.conf"):
        raise ValueError("inference subnet, CNI plugin or resolver path differs")
    host_map = json.loads((cni / "conf/dataset-hosts.json").read_bytes())
    if set(host_map) != ENDPOINTS or not all(ipaddress.ip_address(addr).is_global for addr in host_map.values()):
        raise ValueError("unexpected inference endpoints")
    if (cni / "resolv.conf").read_text().split() != ["nameserver", f"10.235.{match[1]}.1"]:
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
