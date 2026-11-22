#!/usr/bin/env bash
# Run the reviewed TeX recording twice on a single patched reporting stack.
set -euo pipefail
work=$(realpath "$1")
unit=$2
asset=$(realpath "$3")
scripts=$(realpath "$4")
config="$work/runtime.json"
tmp=$(mktemp -d "$asset/runs/reporting-profile.XXXXXXXX")
cp -p "$config" "$tmp/original.json"
restore() {
    cp -p "$tmp/original.json" "$config.next"
    mv "$config.next" "$config"
}
trap restore EXIT
for v in v1 v2; do
    run="337f0b12-$v-toggle-01"
    test ! -e "$asset/runs/$run"
    python3 - "$v" "$config" <<'PY'
import json,os,pathlib,sys
p=pathlib.Path(sys.argv[2]);v=json.loads(p.read_text())
v['free_page_reporting']=sys.argv[1]=='v2'
tmp=p.with_suffix(".next");tmp.write_text(json.dumps(v,indent=2)+"\n");os.replace(tmp,p)
PY
    cp "$config" "$tmp/$v-profile.json"
    bash "$scripts/run-reviewed-pilot.sh" "$work" "$unit" "$asset" "$v" "$run"
done
