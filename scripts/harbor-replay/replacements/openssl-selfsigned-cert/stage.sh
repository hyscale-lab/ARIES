#!/usr/bin/env bash
# Assemble private launch assets, excluding images, guest files and trace data.
set -euo pipefail
repo=$(realpath "$1")
sources=$(realpath "$2")
binary=$(realpath "$3")
tracer=$(realpath "$4")
output=$(realpath -m "$5")
profile=$(cd -- "$(dirname -- "$0")" && pwd)
umask 077
mkdir "$output"
install -d -m 0700 "$output"/{sources,tracer,runs}
cp "$profile"/{source.json,worker.py,run-worker.sh} "$output/"
if [[ -n ${6:-} ]]; then
    install -m 0600 "$(realpath "$6")" "$output/circuit-patch-debs.tar.gz"
fi
cp "$repo/scripts/harbor-replay/audit-idle-runtime.py" "$repo/scripts/zb-trace/vmm_exclusivity.py" "$output/"
install -m 0700 "$binary" "$output/aries-openssl-replacement"
sha256sum "$output/aries-openssl-replacement" | cut -d' ' -f1 >"$output/adapter.sha256"
python3 -B - "$output" "$sources" "$tracer" <<'PY'
import json
from pathlib import Path
import runpy
import shutil
import sys

output, sources, tracer = map(Path, sys.argv[1:])
contract = runpy.run_path(str(output / "worker.py"))
for name, sha in contract["TRACE"].items():
    assert contract["sha"](tracer / name) == sha
    shutil.copy2(tracer / name, output / "tracer" / name)
for row in json.loads((output / "source.json").read_bytes())["reviewed"]["trials"]:
    trial = row["trial"]
    destination = output / "sources" / trial
    destination.mkdir()
    for key, suffix in (("trajectory", "json"), ("rollout", "jsonl")):
        name = f"{key}.{suffix}"
        source = sources / trial / name
        assert contract["sha"](source) == row[f"{key}_sha256"]
        shutil.copyfile(source, destination / name)
PY
(
    cd "$output"
    find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum >SHA256SUMS
    sha256sum --quiet -c SHA256SUMS
)
