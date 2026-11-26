#!/usr/bin/env bash
# Run the published synthetic services on a guest-local bridge with no uplink.
set -euo pipefail
action=${1:?action} run=${2:?prepared directory} sandbox=${3:?sandbox}
meta="${TB41_FIXTURE_DIR:?}/metadata.json"
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
case "$action" in
start)
    fixture=${TB41_PAYMENTS_SIDECARS:?}
    here=$(dirname "$0")
    (cd "$fixture" && sha256sum -c "$here/payments-sidecars.sha256") >"$run/payments-fixture-hashes.txt"
    pid=$(jq -er '.pid | select(.>0)' "$run/workload-state.json")
    mkdir -m700 "$run/payments"
    guest mkdir -p /payments/net-tools
    paths=(/usr/sbin/ip)
    mapfile -t libs < <(LC_ALL=C ldd "${paths[@]}" | awk '$2=="=>" && $3 ~ /^\// {print $3} /^[[:space:]]*\// && NF>1 {print $1}' | sort -u)
    tar -chzf "$run/payments/net-tools.tgz" --transform='s,.*/,,' "${paths[@]}" "${libs[@]}"
    "$AENV_CLI" upload "$sandbox" "$run/payments/net-tools.tgz" /payments/net-tools.tgz
    guest tar -xzf /payments/net-tools.tgz -C /payments/net-tools
    guest rm /payments/net-tools.tgz
    guest bash -ec '
        pid=$1
        d=/payments/net-tools
        ip() { "$d/ld-linux-x86-64.so.2" --library-path "$d" "$d/ip" "$@"; }
        ip link add paybr type bridge
        ip link set paybr up
        for role in kafka customer seeder workload; do
            case "$role" in kafka) n=2;; customer) n=3;; seeder) n=5;; workload) n=4;; esac
            if [[ "$role" == workload ]]; then
                ip link add "pay$n" type veth peer name eth0 netns "$pid"
                net=(nsenter -t "$pid" -n --)
            else
                ip netns add "$role"
                ip link add "pay$n" type veth peer name eth0 netns "$role"
                net=(nsenter --net="/run/netns/$role" --)
            fi
            ip link set "pay$n" master paybr
            ip link set "pay$n" up
            for link in lo eth0; do "${net[@]}" "$d/ld-linux-x86-64.so.2" --library-path "$d" "$d/ip" link set "$link" up; done
            "${net[@]}" "$d/ld-linux-x86-64.so.2" --library-path "$d" "$d/ip" addr add "172.18.0.$n/24" dev eth0
            "${net[@]}" "$d/ld-linux-x86-64.so.2" --library-path "$d" "$d/ip" route show
        done
        ip -o link show master paybr
        test -z "$(ip -4 addr show dev paybr)"
    ' _ "$pid" >"$run/payments-guest-network.txt"
    for role in kafka customer seeder; do
        bundle=/payments/$role
        guest mkdir -p "$bundle/rootfs"
        "$AENV_CLI" upload "$sandbox" "$fixture/$role.rootfs.tar.gz" "$bundle/fs.tgz"
        guest sh -ec 'tar xzf "$1/fs.tgz" -C "$1/rootfs"; rm "$1/fs.tgz"; printf "%s\n" "127.0.0.1 localhost" "172.18.0.2 kafka" "172.18.0.3 customer" >"$1/rootfs/etc/hosts"; : >"$1/rootfs/etc/resolv.conf"' _ "$bundle"
        jq --slurpfile process "$fixture/$role.process.json" --arg role "$role" '
            .process += $process[0] | .hostname=$role | .linux.cgroupsPath=("/zb-"+$role) |
            (.linux.namespaces[] | select(.type=="network")).path=("/run/netns/"+$role)
        ' "$run/config.json" >"$run/payments/$role-spec.json"
        "$AENV_CLI" upload "$sandbox" "$run/payments/$role-spec.json" "$bundle/config.json"
    done
    guest sh -ec '
        wait_for() { i=0; until "$@"; do i=$((i+1)); test "$i" -lt 60 || return 1; sleep 1; done; }
        for role in kafka customer; do
            runc run -d --bundle "/payments/$role" "zb-$role" </dev/null >"/tmp/payments-$role.log" 2>&1
            if test "$role" = kafka; then wait_for runc exec zb-kafka nc -z localhost 9092
            else wait_for runc exec zb-customer curl -fs http://localhost:9000/health; fi
        done
        timeout 180 runc run --bundle /payments/seeder zb-seeder >/tmp/payments-seeder.log 2>&1
        cat /tmp/payments-seeder.log
    ' >"$run/payments-seed.txt"
    ;;
health)
    phase=${4:?phase}
    guest runc exec zb-workload python -c "$(jq -er .payments_health "$meta")" >"$run/payments-$phase-health.json"
    for role in workload kafka customer; do
        guest runc state "zb-$role" >"$run/payments/$role-$phase-state.json"
        jq -e '.status=="running"' "$run/payments/$role-$phase-state.json" >/dev/null
    done
    guest sh -ec 'for role in workload kafka customer; do printf "%s " "$role"; cat "/sys/fs/cgroup/zb-$role/memory.current"; done' >"$run/payments-$phase-cgroup-bytes.txt"
    ;;
*) exit 2;;
esac
