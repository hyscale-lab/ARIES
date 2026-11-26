#!/usr/bin/env bash
# Run inside the staged stock service's network namespace.
set -euo pipefail
: "${1:?task}" "${2:?new attempt label}" "${3:?stock service}" "${4:?expected final artifact SHA-256}"
[[ "$2" =~ ^[a-zA-Z0-9-]+$ && "$3" =~ ^[a-zA-Z0-9-]+$ && "$4" =~ ^[a-f0-9]{64}$ ]]
base=/local/zb-study3-stock
cd "$base"
export TB41_FIXTURE_DIR="$base/$1" XDG_CONFIG_HOME="$base/$3/xdg" AENV_CLI="$base/cli-bundle/aenv"
export STOCK_BASE_IMAGE=127.0.0.1:18762/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a
task=$(jq -er .task "$TB41_FIXTURE_DIR/metadata.json")
[[ "$1" == "$task" ]]
image=127.0.0.1:18762/library/$task@$(jq -er .image_digest "$TB41_FIXTURE_DIR/metadata.json")
test ! -e "$2-prepared"
test ! -e "$2-run"
check_empty() { "$AENV_CLI" list --output json | jq -e 'length==0' >/dev/null; }
check_empty
cleanup() {
    local file id
    for file in "$2-prepared/restored-id" "$2-prepared/original-id"; do
        if [[ -s "$file" ]]; then
            id=$(cat "$file")
            if "$AENV_CLI" list --output json | jq -e --arg id "$id" 'any(.[]; .sandboxID == $id)' >/dev/null; then
                "$AENV_CLI" delete "$id" || return 1
            fi
        fi
    done
    check_empty
}
trap 'status=$?; cleanup "$1" "$2" || exit 1; exit "$status"' EXIT
"$base/prepare-pilot.sh" tb41 "$image" "$base/runc" "$base/$2-prepared"
sandbox=$(cat "$2-prepared/restored-id")
snapshot=$(awk '/^Created snapshot /{print $3}' "$2-prepared/snapshot.txt")
"$AENV_CLI" list --output json | jq -e --arg id "$sandbox" --arg snap "$snapshot" 'length==1 and .[0].sandboxID==$id and .[0].templateID==$snap' >/dev/null
mapfile -t vm < <(ps --ppid "$(cat "$3/server.pid")" -o pid=,comm= | while read -r pid comm; do
    if [[ "$comm" == firecracker ]] && ls -l "/proc/$pid/fd" | grep 'anon_inode:kvm-vcpu' >/dev/null; then echo "$pid"; fi
done)
[[ ${#vm[@]} == 1 ]]
socket=$(tr '\0' '\n' <"/proc/${vm[0]}/cmdline" | awk 'found {print; exit} $0=="--api-sock" {found=1}')
test -S "$socket"
printf '%s\n' "${vm[0]}" "$socket" >"$2-prepared/vmm-binding"
python3 -B "$base/scripts/zb-trace/stock/run-pilot.py" "$base/$2-prepared" "$base/$2-run" --service "$3" --deep --socket "$socket" --tb41-dir "$TB41_FIXTURE_DIR"
read -r actual _ <"$2-run/output.sha256"
test "$actual" = "$4"
