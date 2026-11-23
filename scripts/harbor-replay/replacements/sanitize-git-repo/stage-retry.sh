#!/usr/bin/env bash
set -euo pipefail
repo=$(realpath "$1")
source=$(realpath "$2")
binary=$(realpath "$3")
tracer=$(realpath "$4")
output=$(realpath -m "$5")
profile=$(cd -- "$(dirname -- "$0")" && pwd)
umask 077
mkdir -p "$output"
install -d -m 0700 "$output"/{sources,tracer,runs}
cp "$profile"/{retry-source.json,run-retry.sh,retry-worker.py,retry-controller.sh,retained-original.py,network.sh} "$output/"
cp "$repo/scripts/harbor-replay/audit-idle-runtime.py" "$repo/scripts/zb-trace/vmm_exclusivity.py" "$output/"
install -m 0700 "$binary" "$output/aries-sanitize-retry"
sha256sum "$output/aries-sanitize-retry" | cut -d' ' -f1 >"$output/adapter.sha256"
python3 -B - "$output" "$source" "$tracer" "$repo/scripts/harbor-replay/deep-expansion/trace-contract.py" "${6:-}" <<'PY'
import hashlib
import json
from pathlib import Path
import runpy
import shutil
import subprocess
import sys

out, source, tracer, trace_contract = map(Path, sys.argv[1:5])
profile = json.loads((out / "retry-source.json").read_bytes())
tree = None
if sys.argv[5]:
    proof = json.loads(Path(sys.argv[5]).read_bytes())
    assert profile["image"].endswith("@" + proof["image_digest"])
    tree = proof["reference_clean_tree_sha256"]
    assert len(tree) == 64 and all(c in "0123456789abcdef" for c in tree)
    (out / "clean-tree-proof.json").write_text(json.dumps(proof, indent=2) + "\n")
trace = runpy.run_path(str(trace_contract))["TRACE"]
(out / "trace-digests.json").write_text(json.dumps(trace, indent=2) + "\n")
def sha(path):
    with path.open("rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()
for name, digest in trace.items():
    assert sha(tracer / name) == digest
    shutil.copy2(tracer / name, out / "tracer" / name)
expected = {}
for row in profile["trials"]:
    trial = row["trial"]
    destination = out / "sources" / trial
    destination.mkdir()
    for key, suffix in (("trajectory", "json"), ("rollout", "jsonl")):
        name = f"{key}.{suffix}"
        artifact = source / trial / name
        assert sha(artifact) == row[f"{key}_sha256"]
        shutil.copyfile(artifact, destination / name)
    compiled = json.loads(subprocess.check_output([
        out / "aries-sanitize-retry", "-trajectory", destination / "trajectory.json", "-rollout", destination / "rollout.jsonl",
    ]))
    assert compiled["needs_network"]
    guest = {"exec_command", "write_stdin", "view_image"}
    calls = [e for e in compiled["events"] if e["kind"] in guest]
    assert calls and {e["guest_sequence"] for e in calls} == set(range(1, len(calls) + 1))
    expected[trial[:8]] = dict(trial=row, guest_calls=len(calls))
    if tree is not None:
        expected[trial[:8]]["verified_clean_tree_sha256"] = tree
(out / "expected-calls.json").write_text(json.dumps(expected, indent=2) + "\n")
PY
if [[ -n ${7:-} ]]; then
    cache=$(realpath "$7")
    install -d -m 0700 "$output/fixtures/net"
    install -m 0700 "$cache/aries-harbor-image" "$output/fixtures/aries-harbor-image"
    install -m 0600 "$cache/dataset-apt/curl-debs.tar.gz" "$output/fixtures/curl-debs.tar.gz"
    cp -a "$cache/cni/"{bin,SHA256SUMS} "$output/fixtures/net/"
    (
        cd "$output/fixtures"
        sha256sum --quiet -c <<'SUMS'
b06faaf4f96c04e41d6808ee8e38317ea1fd19c68b20693b2045c8fe7e712bc9  aries-harbor-image
6e8e1dac4bc58e974f2654b53c6e4d5210cc8b7f492f1ad5660cdab5ac4c6998  curl-debs.tar.gz
SUMS
        cd net/bin
        sha256sum --quiet -c ../SHA256SUMS
    )
fi
(
    cd "$output"
    find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >SHA256SUMS
    sha256sum --quiet -c SHA256SUMS
)
