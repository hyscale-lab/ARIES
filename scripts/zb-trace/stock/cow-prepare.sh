#!/usr/bin/env bash
# Run inside the isolated service's mount and network namespaces.
set -euo pipefail
umask 077
label=${1:?service label}
task=${2:?base or workload}
[[ "$label" =~ ^[a-z0-9-]+$ ]]
b=/local/zb-study3-stock
t="$b/$label-tools"
stock="$t/zb-trace/stock"
control="/tmp/zb-$label.sock"
export AENV_CLI="$b/cli-bundle/aenv" XDG_CONFIG_HOME="$b/$label/xdg"
export STOCK_BASE_IMAGE=127.0.0.1:18762/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a
store="$b/$label/home/snapshot-store/repository"
gate() { python3 "$t/cow-gate.py" "$1" "$control" "${@:2}"; }
fc() { curl -fsS --unix-socket "$socket" "http://localhost/$1"; }
mkdir -p "$t/ready"
if [[ "$task" == base ]]; then
    r="$t/ready/base"
    mkdir "$r"
    if [[ ! -s "$t/base.id" ]]; then
        "$AENV_CLI" start --cold -d --timeout 3600 --cpu 2 --memory 4096 "$STOCK_BASE_IMAGE" >"$t/base.id"
    fi
    sandbox=$(cat "$t/base.id")
    "$AENV_CLI" exec "$sandbox" sh -ec '
        test ! -e /workload
        if command -v runc; then exit 1; fi
        uname -r; cat /proc/sys/kernel/random/boot_id
        cat /sys/module/page_reporting/parameters/page_reporting_order
        cat /sys/module/damon_reclaim/parameters/enabled
        cat /proc/cmdline; cat /proc/mounts
    ' >"$r/guest.txt"
    gate list >"$r/vmms.json"
    candidates=()
    while read -r candidate; do
        if curl -fsS --unix-socket "$candidate" http://localhost/ | jq -e '.state=="Running"' >/dev/null; then
            candidates+=("$candidate")
        fi
    done < <(jq -r '.[] | select(.state!="exited") | .real_socket' "$r/vmms.json")
    [[ ${#candidates[@]} == 1 ]]
    socket=${candidates[0]}
    python3 "$stock/cow-sample.py" inspect "$socket" "$r/inspect.json"
    fc vm/config >"$r/vm-config.json"
    sha256sum "$b/$label/home/deps/kernel/vmlinux-6.18/vmlinux.bin" >"$r/kernel.sha256"
    "$AENV_CLI" snapshot create "$sandbox" --name "$label-common" >"$r/snapshot.txt"
    "$AENV_CLI" delete "$sandbox"
else
    export CALENDAR_INPUTS="$t/fixtures/calendar" LOCAL_SOL_DIR="$t/fixtures"
    wild=127.0.0.1:18762/library/wildclaw@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078
    case "$task" in
        session-window-debug|wal-recovery-ordering)
            type=tb41
            export TB41_FIXTURE_DIR="$t/fixtures/$task"
            fixture=/local/zb-tb41-cohort/wave10/fixtures
            export TB41_CODEX_ARCHIVE="$fixture/codex-harbor-tools.tar.gz"
            if [[ "$task" == session-window-debug ]]; then
                export TB41_PACKAGE_ARCHIVE="$fixture/window-os-tools.tar.gz"
            else
                export TB41_PACKAGE_ARCHIVE="$fixture/os-tools.tar.gz"
            fi
            image="127.0.0.1:18762/library/$task@$(jq -er .image_digest "$TB41_FIXTURE_DIR/metadata.json")";;
        calendar|chapter-summary) type=$task; image=$wild;;
        *) exit 2;;
    esac
    r="$t/ready/$task"
    mkdir "$r"
    ancestor=$(awk '/^Created snapshot /{print $3}' "$t/ready/base/snapshot.txt")
    test -n "$ancestor"
    printf '%s\n' "$ancestor" >"$r/ancestor-id"
    "$AENV_CLI" start -d --timeout 3600 --image-mount "/workload/rootfs=$image" "$ancestor" >"$r/sandbox-id" &
    start=$!
    for _ in $(seq 900); do
        held=$(gate list | jq -c '[.[]|select(.state=="held")]')
        if [[ "$(jq 'length' <<<"$held")" != 0 ]]; then break; fi
        kill -0 "$start" || { wait "$start"; exit 1; }
        sleep .1
    done
    jq -e 'length==1 and .[0].load.resume_vm==false and .[0].load.track_dirty_pages==true' <<<"$held" > /dev/null
    printf '%s\n' "$held" >"$r/held.json"
    socket=$(jq -er '.[0].real_socket' <<<"$held")
    pid=$(jq -er '.[0].pid' <<<"$held")
    python3 "$stock/cow-sample.py" inspect "$socket" "$r/inspect.json"
    fc vm/config >"$r/vm-config.json"
    gate release "$pid" >"$r/release.json"
    wait "$start"
    export COW_PREPARE_SANDBOX
    COW_PREPARE_SANDBOX=$(cat "$r/sandbox-id")
    "$AENV_CLI" list --output json | jq -e --arg id "$COW_PREPARE_SANDBOX" --arg base "$ancestor" 'any(.[];.sandboxID==$id and .templateID==$base)' >/dev/null
    "$stock/prepare-pilot.sh" "$type" "$image" "$b/runc" "$r/prepared" >"$r/prepare.log" 2>&1
    cp "$r/prepared/snapshot.txt" "$r/snapshot.txt"
    # This exact stock GET follows state-only snapshot create while the vCPUs are paused.
    archive=$(jq -sr --argjson pid "$pid" '[.[] | select(.event=="dirty_ranges" and .pid==$pid)] | if length==1 then .[0].archive else error("ambiguous native snapshot response") end' "$t/gate.jsonl")
    python3 "$stock/cow-sample.py" setup-from-receipt "$r/inspect.json" "$t/$archive" "$r/setup.json"
fi
snapshot=$(awk '/^Created snapshot /{print $3}' "$r/snapshot.txt")
test -n "$snapshot"
cp "$store/catalog/records/$snapshot.json" "$r/catalog.json"
jq -e '.committed.memory_startup == null' "$r/catalog.json" >/dev/null
ls "$store/snapshots/$snapshot" >"$r/artifacts.txt"
! grep -q 'memory-startup.pack' "$r/artifacts.txt"
