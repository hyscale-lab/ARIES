#!/usr/bin/env bash
# Start in a fresh mount and network namespace as root, with stock binaries staged by stage-stock.sh.
set -euo pipefail
umask 077
base=/local/zb-study3-stock
label=${1:?new service label}
[[ "$label" =~ ^[a-z0-9-]+$ ]]
tools="$base/$label-tools"
cd "$base"
test ! -e "$label"
export COW_GATE_SOCKET="/tmp/zb-$label.sock"
chmod 711 "$tools"
export COW_GATE_FIRECRACKER="$base/$label/home/deps/firecracker/1.15.1-patch-v2/firecracker"
sha256sum -c <<CHECK
19b402c82e4ef16c12f2434d579448d933ac943f099c993581226d8b6d8f30ea  server-bundle/server
cad0cd9291e7364dff7270981b277e0094b2d5ab76dbd17e16bbe9e41def9875  server-bundle/deps/firecracker/1.15.1-patch-v2/firecracker
CHECK
# Keep mutable cache state private. The hard links below reference immutable layer commits.
mkdir "$tools/cache"
for source in "$base/image-cache" "$base/stock189/home/image-cache"; do
    cp -aln "$source/commits" "$tools/cache/"
    cp -an "$source/indexes" "$source/configs" "$tools/cache/"
done
for file in "$tools"/cache/configs/*-image.json; do
    sed -Ei 's@/local/zb-study3-stock/([^"]*/)?image-cache/commits/@'"$tools"'/cache/commits/@g' "$file"
done
python3 - "$tools" <<'PY'
import sys,tomllib
from pathlib import Path
tools=Path(sys.argv[1])
s=Path('server-bundle/default.toml').read_text()
s=s.replace('[firecracker]\n', f'[firecracker]\nbinary_path = "{tools}/cow-gate.py"\n')
s=s.replace('[image.cache]\n', f'[image.cache]\nroot_dir = "{tools}/cache"\n')
s=s.replace('# [snapshot.memory_startup_pack]', '[snapshot.memory_startup_pack]').replace('# enabled = false\n# record_min', 'enabled = false\n# record_min').replace('# consume_enabled = false', 'consume_enabled = false')
start=s.index('[memory_snapshot.background_download]')
s=s[:start]+s[start:].replace('enable = true','enable = false',1)
cfg=tomllib.loads(s)
assert cfg['snapshot']['memory_startup_pack']=={'enabled':False,'consume_enabled':False}
assert cfg['memory_snapshot']['track_dirty_pages'] and not cfg['memory_snapshot']['background_download']['enable']
assert 'page_reporting.page_reporting_order=6' in cfg['firecracker']['boot_args']
assert cfg['firecracker']['binary_path']==str(tools/'cow-gate.py')
(tools/'effective.toml').write_text(s)
PY
python3 "$tools/cow-gate.py" serve "$COW_GATE_SOCKET" "$tools/gate.jsonl" &
gate=$!
trap 'kill -TERM "$gate" 2>/dev/null || true' EXIT
for _ in {1..100}; do [[ ! -S "$COW_GATE_SOCKET" ]] || break; sleep .05; done
test -S "$COW_GATE_SOCKET"
bash "$tools/smoke.sh" "$label" serve "$tools/effective.toml"
