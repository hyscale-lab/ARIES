#!/usr/bin/env bash
# Give the third original a private DNS and HTTPS origin with no upstream route.
set -euo pipefail
stage=$(realpath "$1")
action=$2
bridge=zbs3hist200
network=zb-shard-history-200
subnet=10.251.200.0/28
gateway=10.251.200.1
chain=ZBS3HIST200
dns=zb-shard-history-dns-amd200
https=zb-shard-history-https-amd200
if [[ "$action" == remove ]]; then
    [[ -z "$(ip -o link show master "$bridge")" ]]
    systemctl stop "$https" "$dns"
    iptables -w -t mangle -D PREROUTING -i "$bridge" -j "$chain"
    iptables -w -t mangle -F "$chain"
    iptables -w -t mangle -X "$chain"
    ip6tables -w -t mangle -D PREROUTING -i "$bridge" -j DROP
    ip link delete dev "$bridge" type bridge
    exit
fi
[[ "$action" == install ]]
! ip link show dev "$bridge" >/dev/null 2>&1
! iptables -w -t mangle -S "$chain" >/dev/null 2>&1
! systemctl is-active --quiet "$dns" "$https"
mkdir -p "$stage"/{conf,ipam,cache}
python3 - "$stage" "$network" "$subnet" "$bridge" "$gateway" <<'PY'
import ipaddress, json, pathlib, subprocess, sys
base = pathlib.Path(sys.argv[1])
name, subnet, bridge, gateway = sys.argv[2:]
candidate = ipaddress.ip_network(subnet)
routes = json.loads(subprocess.check_output(["ip","-j","-4","route"]))
assert not any(candidate.overlaps(ipaddress.ip_network(route["dst"],strict=False))
               for route in routes if route.get("dst","default") != "default")
(base/"resolv.conf").write_text("nameserver " + gateway + "\n")
config = {"cniVersion":"1.0.0","name":name,"plugins":[{"type":"bridge","bridge":bridge,
          "isGateway":True,"forceAddress":False,"ipMasq":False,"hairpinMode":True,"mtu":1500,
          "ipam":{"type":"host-local","subnet":subnet,"dataDir":str(base/"ipam"),
                  "resolvConf":str(base/"resolv.conf")}},{"type":"firewall"},
          {"type":"tc-redirect-tap"},{"type":"loopback"}]}
(base/"conf"/"10-shard-history.conflist").write_text(json.dumps(config,indent=2)+"\n")
PY
ip link add name "$bridge" type bridge
ip addr add "$gateway/28" dev "$bridge"
ip link set dev "$bridge" up
iptables -w -t mangle -N "$chain"
iptables -w -t mangle -A "$chain" ! -s "$subnet" -j DROP
iptables -w -t mangle -A "$chain" -d "$gateway" -p udp --dport 53 -j RETURN
iptables -w -t mangle -A "$chain" -d "$gateway" -p tcp -m multiport --dports 53,443 -j RETURN
iptables -w -t mangle -A "$chain" -j DROP
iptables -w -t mangle -I PREROUTING 1 -i "$bridge" -j "$chain"
ip6tables -w -t mangle -I PREROUTING 1 -i "$bridge" -j DROP
systemd-run --unit="$dns" /usr/sbin/dnsmasq --keep-in-foreground --conf-file=/dev/null \
    --bind-interfaces --interface="$bridge" --except-interface=lo --no-hosts --no-resolv \
    "--listen-address=$gateway" --local=/#/ \
    "--address=/raw.githubusercontent.com/s3.amazonaws.com/www.gutenberg.org/$gateway"
systemd-run --unit="$https" --property="StandardOutput=append:$stage/https.jsonl" \
    /usr/bin/python3 -B "$stage/serve-shard-history.py" "$stage/payloads" "$stage/cert.pem" "$stage/key.pem"
for host in raw.githubusercontent.com s3.amazonaws.com www.gutenberg.org; do
    health=$(curl --fail --silent --show-error --retry 4 --retry-connrefused --noproxy '*' \
        --cacert "$stage/cert.pem" --resolve "$host:443:$gateway" "https://$host/healthz")
    [[ "$health" == shard-history ]]
done
