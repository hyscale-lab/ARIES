#!/usr/bin/env bash
# Run one solo WildClaw image-category arm on its exclusively assigned worker.
set -euo pipefail
umask 077
if (($# < 2 || $# > 3)); then echo 'usage: image-category-arm.sh 6|9|1|pack ATTEMPT [link-pix]' >&2; exit 2; fi
base=/local/zb-study3-stock
here=$(dirname "$(realpath "$0")")
image=127.0.0.1:18762/library/wildclaw@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078
trace="$base/01_Productivity_Flow_task_8_real_image_category.jsonl"
[[ "$2" =~ ^[a-zA-Z0-9-]+$ ]]
task=${3:-image-category}
case "$task" in
    image-category) service="image-$1-$2"; source_sha=2543f544fef10d6ee0da62095eb37bdfc9ba9f4d5998db798e7a54e85c0918ab; cache="$base/image-cache";;
    link-pix) service="puz-$1-$2"; source_sha=0f37ca65135cf0513ada9eef9fe1cc62dc8751cdc0f7fa872ad06d6e68e27709
        trace="$base/02_Code_Intelligence_task_9_link_a_pix_color_easy_zh.jsonl"; cache="$base/live/home/image-cache";;
    schedule-edit|trash-fixture|chapter-summary)
        case "$task" in
            schedule-edit) prefix=se; suffix=4_authority; source_sha=b81663dd33480607ca01403d075a5a2cfa10a1fce0945df9d0c17cb9ac597f25;;
            trash-fixture) prefix=tf; suffix=5_risk_os_operation; source_sha=653b6bbe76655ec283f849e2d4f731f78e5b7adb78ea11ff4cbc833d297db0fe;;
            chapter-summary) prefix=cs; suffix=6_prompt_injection; source_sha=dce6d103d9d7fb344b44a6e4b37bab46461e92f89e8960ca3af5907931f1c3bd;;
        esac
        service="$prefix-$1-$2"; trace="$base/06_Safety_Alignment_task_$suffix.jsonl"
        cache="$base/live/home/image-cache"; [[ -d "$cache" ]] || cache="$base/image-cache";;
    pdf-digest) service="pdf-$1-$2"; source_sha=5d721acdbeeedc7cb51a7779d7a8328756422f2d512620bcb400a5f50663be1f
        trace="$base/01_Productivity_Flow_task_10_pdf_digest.jsonl"; cache="$base/image-cache";;
    conflicting-handling) service="law-$1-$2"; source_sha=7a020f97fe201c8153c08e219839b7a6b6d77ae6f6755a42588c27a2f42fef39
        trace="$base/04_Search_Retrieval_task_2_conflicting_handling.jsonl"; cache="$base/live/home/image-cache"
        [[ -d "$cache" ]] || cache="$base/image-cache";;
    *) exit 2;;
esac
work="$base/$service"
run="$base/$service-run"
options=(--deep)
store=
case "$1" in
    6|9|1) order=$1; config="$base/server-bundle/default.toml" ;;
    pack) order=6; config="$here/pack.toml"; options+=(--checkpoint)
          store="$work/home/snapshot-store/repository" ;;
    *) exit 2 ;;
esac
mkdir "$run"
sed -e "s/page_reporting_order=6/page_reporting_order=$order/" \
    -e "/^\[image.cache\]$/a root_dir = \"$cache\"" "$config" >"$run/server.toml"
printf '%s  %s\n' "$source_sha" "$trace" | sha256sum -c -
unshare --mount --net --propagation private /bin/bash "$here/smoke.sh" "$service" serve "$run/server.toml" >"$run/service.log" 2>&1 &
launcher=$!
pid=
in_stock() { nsenter -t "$pid" -m -n -- "$@"; }
aenv() { in_stock env XDG_CONFIG_HOME="$work/xdg" "$base/cli-bundle/aenv" "$@"; }
active_vmms() {
    ps --ppid "$pid" -o pid=,comm= | while read -r child name; do
        if [[ "$name" == firecracker ]] && ls -l "/proc/$child/fd" | grep 'anon_inode:kvm-vcpu' >/dev/null; then
            printf '%s\n' "$child"
        fi
    done
}
cleanup() {
    local status=$? ids id
    trap - EXIT
    if [[ -n "$pid" ]]; then
        ids=$(aenv list --output json | jq -r '.[].sandboxID') || status=1
        while IFS= read -r id; do
            if [[ -n "$id" ]]; then aenv delete "$id" >>"$run/cleanup.log" 2>&1 || status=1; fi
        done <<<"$ids"
        aenv list --output json >"$run/list-after-delete.json" || status=1
        jq -e 'length == 0' "$run/list-after-delete.json" >/dev/null || status=1
        [[ -z "$(active_vmms)" ]] || status=1
        kill -TERM "$pid" 2>/dev/null || status=1
    else
        kill -TERM "$launcher" 2>/dev/null || true
    fi
    wait "$launcher" || true
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
for _ in {1..60}; do
    [[ ! -s "$work/server.pid" ]] || break
    kill -0 "$launcher"
    sleep 1
done
pid=$(cat "$work/server.pid")
regctl() { in_stock env REGCTL_CONFIG="$base/registry/regctl.json" "$base/server-bundle/deps/regctl/v0.11.5/regctl" "$@"; }
regctl image digest 127.0.0.1:18762/library/wildclaw:v13 >"$run/image-digest"
cmp "$run/image-digest" <(printf '%s\n' "${image##*@}")
in_stock env STOCK_PACK_STORE="$store" XDG_CONFIG_HOME="$work/xdg" AENV_CLI="$here/calendar-aenv.py" \
    STOCK_AENV_CLI="$base/cli-bundle/aenv" WILDCLAW_IMAGES_TAR="$base/images.tar" PUZZLE_FIXTURE="$base/link-pix" \
    STOCK_BASE_IMAGE=127.0.0.1:18762/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a \
    "$here/prepare-pilot.sh" "$task" "$image" "$base/runc" "$run/prepared" >"$run/prepare.log" 2>&1
sandbox=$(cat "$run/prepared/restored-id")
snapshot=$(awk '/^Created snapshot / { print $3 }' "$run/prepared/snapshot.txt")
cmp "$run/prepared/"{workload-state,restored-workload-state}.json
aenv exec "$sandbox" sh -ec '
    cat /sys/module/page_reporting/parameters/page_reporting_order
    cat /sys/module/damon_reclaim/parameters/enabled
    blockdev --getsize64 /dev/vda
    blockdev --getsize64 "$(findmnt -n -o SOURCE /workload/rootfs)"
' >"$run/guest-restored-probe.txt"
diff -u - "$run/guest-restored-probe.txt" < <(printf '%s\nY\n68719476736\n68719476736\n' "$order")
aenv list --output json >"$run/list-before-replay.json"
jq -e --arg sandbox "$sandbox" --arg snapshot "$snapshot" \
    'length == 1 and .[0].sandboxID == $sandbox and .[0].templateID == $snapshot' "$run/list-before-replay.json" >/dev/null
mapfile -t vmms < <(active_vmms)
[[ "${#vmms[@]}" == 1 ]]
vm=${vmms[0]}
socket=$(tr '\0' '\n' <"/proc/$vm/cmdline" | awk 'seen { print; exit } $0=="--api-sock" { seen=1 }')
printf '%s\n' "$vm" "$socket" >"$run/vmm-binding"
curl -fsS --unix-socket "$socket" http://localhost/ >"$run/firecracker-info.json"
sha256sum "${STOCK_REPLAY_BINARY:-$base/aries-agentenv-replay}" "$base"/{native-deep-scan,zb-guest,runc} \
    "$here"/{run-pilot,deep,host_sample}.py >"$run/binary-and-observer-sha256.txt"
in_stock python3 "$here/run-pilot.py" "$run/prepared" "$run/traced" --service "$service" \
    --wildclaw "$trace" --socket "$socket" "${options[@]}" >"$run/traced.stdout" 2>"$run/traced.stderr"
if [[ "$task" == link-pix ]]; then
    if [[ -s "$run/traced/final-sandbox-id" ]]; then sandbox=$(cat "$run/traced/final-sandbox-id"); fi
    aenv exec "$sandbox" runc exec zb-workload python3 -c 'from PIL import Image; import hashlib; im=Image.open("/tmp_workspace/results/result.png"); assert im.size==(600,600) and im.mode=="RGB"; print(hashlib.sha256(im.tobytes()).hexdigest())' >"$run/traced/pixel-sha256"
    python3 "$here/link-pix-check.py" "$trace" "$run/traced" >"$run/traced/validation.json"
elif [[ "$task" == schedule-edit || "$task" == trash-fixture || "$task" == chapter-summary ]]; then
    python3 "$here/local-files-check.py" "$trace" "$run/traced" >"$run/traced/validation.json"
elif [[ "$task" == pdf-digest || "$task" == conflicting-handling ]]; then
    python3 "$here/document-check.py" "$trace" "$run/traced" "$base/local-sol/$task/environment.tar" >"$run/traced/validation.json"
else
    python3 "$here/image-category-check.py" "$trace" "$base/images.tar" "$run/traced" >"$run/traced/validation.json"
fi
