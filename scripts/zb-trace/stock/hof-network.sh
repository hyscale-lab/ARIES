#!/usr/bin/env bash
# Functions called only inside a fresh HOF service or during HOF guest preparation.
hof_hosts=(api.github.com github.com raw.githubusercontent.com release-assets.githubusercontent.com snapshot.ubuntu.com)

homepage_hosts=(github.com api.github.com registry.npmjs.org cdn.playwright.dev playwright.download.prss.microsoft.com storage.googleapis.com)
nextjs_hosts=(registry.npmjs.org snapshot.debian.org cdn.playwright.dev storage.googleapis.com playwright.download.prss.microsoft.com)

hof_start_endpoints() {
    local label=$1 pid=$2 host address target
    local number=10 endpoint_pid
    # The stock firewall distinguishes routed public traffic from traffic addressed to the service host.
    unshare --net -- sleep infinity &
    endpoint_pid=$!
    hof_proxies+=("$endpoint_pid")
    for _ in $(seq 50); do
        [[ "$(readlink "/proc/$endpoint_pid/ns/net")" == "$(readlink /proc/self/ns/net)" ]] || break
        sleep .02
    done
    local fixture=(nsenter -t "$endpoint_pid" -n --)
    local service=(nsenter -t "$pid" -n --)
    "${fixture[@]}" ip link set lo up
    "${service[@]}" ip link add hof-egress type veth peer name hof-origin netns "$endpoint_pid"
    "${service[@]}" ip address add 192.0.2.245/30 dev hof-egress
    "${service[@]}" ip link set hof-egress up
    "${fixture[@]}" ip address add 192.0.2.246/30 dev hof-origin
    "${fixture[@]}" ip link set hof-origin up
    for host in "${hof_hosts[@]}"; do
        number=$((number + 1))
        address="192.0.2.$number"
        target=$(python3 - "$host" <<'PY'
import ipaddress, socket, sys
ips = [item[4][0] for item in socket.getaddrinfo(sys.argv[1], 443, socket.AF_INET, socket.SOCK_STREAM)]
assert ips and all(ipaddress.ip_address(ip).is_global for ip in ips)
print(ips[0])
PY
)
        "${fixture[@]}" ip address add "$address/32" dev lo
        "${service[@]}" ip route add "$address/32" via 192.0.2.246 dev hof-egress
        # The listener can forward only to this public IP and port. TLS remains end-to-end.
        "${fixture[@]}" systemd-socket-activate -l "$address:443" \
            nsenter --net=/proc/1/ns/net --no-fork /lib/systemd/systemd-socket-proxyd "$target:443" \
            >"$label/$host.proxy.log" 2>&1 &
        hof_proxies+=("$!")
        printf '%s %s %s\n' "$address" "$host" "$target" >>"$label/hof-endpoints.txt"
    done
    export TB41_HOF_ENDPOINTS
    TB41_HOF_ENDPOINTS=$(realpath "$label/hof-endpoints.txt")
    for host in "${hof_hosts[@]}"; do
        address=$(awk -v host="$host" '$2 == host {print $1}' "$label/hof-endpoints.txt")
        nsenter -t "$pid" -n -- curl -sS --connect-timeout 5 --max-time 15 --retry 2 --retry-connrefused \
            --resolve "$host:443:$address" "https://$host/" -o /dev/null
    done
}

hof_network_guest() {
    local sandbox=$1 run=$2 pid archive="$2/hof-net-tools.tar.gz"
    cp "${TB41_HOF_ENDPOINTS:?}" "$run/hof-endpoints.txt"
    pid=$(jq -er '.pid | select(. > 0)' "$run/workload-state.json")
    # Stage the worker's Ubuntu userspace tools outside the workload. Guest kernel and VMM stay stock.
    local paths=("/usr/sbin/ip" "/usr/sbin/nft")
    mapfile -t libs < <(LC_ALL=C ldd "${paths[@]}" | awk '$2=="=>" && $3 ~ /^\// {print $3} /^[[:space:]]*\// && NF > 1 {print $1}' | sort -u)
    tar -chzf "$archive" --transform='s,.*/,,' "${paths[@]}" "${libs[@]}"
    (cd "$run" && sha256sum hof-net-tools.tar.gz >hof-net-tools.sha256)
    "$AENV_CLI" upload "$sandbox" "$archive" /tmp/hof-net-tools.tar.gz
    rm "$archive"
    guest sh -ec 'mkdir /tmp/hof-net-tools; tar -xzf /tmp/hof-net-tools.tar.gz -C /tmp/hof-net-tools; rm /tmp/hof-net-tools.tar.gz'
    guest bash -ec '
        set -eu
        d=/tmp/hof-net-tools
        loader=$d/ld-linux-x86-64.so.2
        ip() { "$loader" --library-path "$d" "$d/ip" "$@"; }
        container_ip() { nsenter -t "$1" -n -- "$loader" --library-path "$d" "$d/ip" "${@:2}"; }
        pid=$1
        ip link add hof-host type veth peer name hof-task netns "$pid"
        ip address add 192.0.2.241/30 dev hof-host
        ip link set hof-host up
        container_ip "$pid" address add 192.0.2.242/30 dev hof-task
        container_ip "$pid" link set hof-task up
        local_addresses=""
        for n in $(seq 11 "$((10 + $2))"); do
            address="192.0.2.$n"
            container_ip "$pid" route add "$address/32" via 192.0.2.241 dev hof-task
            local_addresses="${local_addresses:+$local_addresses,}$address"
        done
        "$loader" --library-path "$d" "$d/nft" -f - <<RULES
table ip hof {
    chain input { type filter hook input priority -10; policy accept; iifname "hof-host" drop; }
    chain forward { type filter hook forward priority -10; policy accept; iifname "hof-host" ip daddr { $local_addresses } tcp dport 443 accept; iifname "hof-host" drop; }
    chain nat { type nat hook postrouting priority srcnat; policy accept; ip saddr 192.0.2.242 masquerade; }
}
RULES
        echo 1 > /proc/sys/net/ipv4/ip_forward
        container_ip "$pid" route show
        for blocked in 169.254.169.254 10.0.0.1; do
            if container_ip "$pid" route get "$blocked"; then exit 1; fi
        done
        "$loader" --library-path "$d" "$d/nft" list table ip hof
        rm -r "$d"
    ' _ "$pid" "${#hof_hosts[@]}" >"$run/hof-network.txt"
    local n=10 host
    for host in "${hof_hosts[@]}"; do
        n=$((n + 1))
        guest sh -ec 'printf "%s %s\n" "$1" "$2" >> /workload/rootfs/etc/hosts' _ "192.0.2.$n" "$host"
    done
}
