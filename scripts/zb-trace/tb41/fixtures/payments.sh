#!/bin/bash
# Start the public synthetic companions outside the measured VM.
set -euo pipefail
stage=$(realpath "$1")
runtime=$(realpath "$2")
unit=$3
arm=$4
out=$(realpath -m "$5")
trial=4d316e1a-590b-41e6-8a2d-a55a895af680
bridge=zbpayments198
sidecars="$out.sidecars"
mkdir -m 0700 "$sidecars"
runc=("$runtime/bin/runc" --root "$sidecars/runc")

# The bridge has no host addresses or uplink. The VM's nominal gateway is unassigned.
test -z "$(ip route show 172.18.0.0/24)"
! ip link show "$bridge" >/dev/null 2>&1
ip link add "$bridge" type bridge
created=()
finish() {
    rc=$?
    trap - EXIT
    for name in "${created[@]}"; do
        if "${runc[@]}" state "$name" >/dev/null 2>&1; then
            "${runc[@]}" delete --force "$name" || rc=1
        fi
        ip netns delete "$name" || rc=1
    done
    ip link delete "$bridge" || rc=1
    "${runc[@]}" list --format json >"$sidecars/remaining.json" || rc=1
    remaining=$(cat "$sidecars/remaining.json")
    [[ "$remaining" == null || "$remaining" == '[]' ]] || rc=1
    printf '%s\n' "$rc" >"$out.exit"
    exit "$rc"
}
trap finish EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
sysctl -q -w "net.ipv6.conf.$bridge.disable_ipv6=1"
ip link set "$bridge" up
test -z "$(ip -o addr show dev "$bridge")"

for item in kafka customer seeder; do
    name="zbp-$item"
    case "$item" in
        kafka) last=2 ;;
        customer) last=3 ;;
        seeder) last=4 ;;
    esac
    ip netns add "$name"
    created+=("$name")
    ip link add "$name" type veth peer name eth0 netns "$name"
    ip link set "$name" master "$bridge"
    ip link set "$name" up
    ip -n "$name" addr add "172.18.0.$last/24" dev eth0
    ip -n "$name" link set eth0 up
    ip -n "$name" link set lo up
    test -z "$(ip -n "$name" route show default)"

    bundle="$sidecars/$item"
    mkdir -p "$bundle/rootfs"
    tar -xzf "$stage/sidecars/$item.rootfs.tar.gz" -C "$bundle/rootfs"
    printf '%s\n' '127.0.0.1 localhost' '172.18.0.2 kafka' '172.18.0.3 customer' >"$bundle/rootfs/etc/hosts"
    : >"$bundle/rootfs/etc/resolv.conf"
    "${runc[@]}" spec --bundle "$bundle"
    python3 - "$stage/sidecars/$item.process.json" "$bundle/config.json" "$item" "$name" <<'PY'
import json
import sys
from pathlib import Path

process_file, config_file = map(Path, sys.argv[1:3])
item, name = sys.argv[3:]
config = json.loads(config_file.read_bytes())
config["hostname"] = item
config["root"]["readonly"] = False
process = config["process"]
process.update(json.loads(process_file.read_bytes()), terminal=False)
for namespace in config["linux"]["namespaces"]:
    if namespace["type"] == "network":
        namespace["path"] = "/run/netns/" + name
config_file.write_text(json.dumps(config))
PY
done

wait_for() {
    for _ in $(seq 1 60); do
        "$@" && return
        sleep 1
    done
    "$@"
}
"${runc[@]}" run --detach --bundle "$sidecars/kafka" zbp-kafka </dev/null >"$sidecars/kafka.log" 2>&1
wait_for "${runc[@]}" exec zbp-kafka nc -z localhost 9092
"${runc[@]}" run --detach --bundle "$sidecars/customer" zbp-customer </dev/null >"$sidecars/customer.log" 2>&1
wait_for "${runc[@]}" exec zbp-customer curl -fs http://localhost:9000/health
timeout 120 "${runc[@]}" run --bundle "$sidecars/seeder" zbp-seeder >"$sidecars/seeder.log" 2>&1
printf '%s\n' 'broker and customer healthy; original seeder exited zero' >"$sidecars/ready.txt"

ZB_SOURCE_DIR="$stage/sources/$trial" ZB_GUEST_PYTHON=/usr/local/bin/python3 \
    bash "$stage/zb-trace/tb41/pilot.sh" "$stage" "$runtime" "$unit" "$arm" "$out" \
    -codex-tools "$stage/fixtures/codex-harbor-tools.tar.gz" \
    -cni-network "$bridge" -cni-conf-dir "$stage/cni/conf" \
    -cni-cache-dir "$stage/cni/cache" -cni-bin-dir "$stage/cni/bin" || exit "$?"

"${runc[@]}" exec zbp-kafka sync
status=0
"${runc[@]}" exec zbp-kafka tar -czf - -C /tmp/kafka-logs . >"$sidecars/kafka-snapshot.tgz" || status=$?
test "$status" -le 1
tar -tzf "$sidecars/kafka-snapshot.tgz" >"$sidecars/kafka-snapshot-files.txt"
