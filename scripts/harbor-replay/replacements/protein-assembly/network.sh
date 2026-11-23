#!/usr/bin/env bash
# Limit the exact source recording to the public service names in its native calls.
set -euo pipefail
base=$(realpath "$1")
action=$2
node=$3
[[ $node == 183 || $node == 198 || $node == 201 ]]
name=zbprot$node
subnet=10.250.$node.0/28
resolver=10.250.$node.1
chain=ZB${node}PROT
unit=zb-protein-dns-amd$node
dns=/run/$unit
ipt=(sudo -n iptables -w -t mangle)
jump=(-i "$name" -s "$subnet" -m comment --comment "$name" -j "$chain")
if [[ $action == remove ]]; then
    if "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
        "${ipt[@]}" -D PREROUTING "${jump[@]}"
        "${ipt[@]}" -F "$chain"
        "${ipt[@]}" -X "$chain"
    fi
    if sudo -n systemctl is-active --quiet "$unit"; then sudo -n systemctl stop "$unit"; fi
    exit 0
fi
test "$action" = install
test "$(sysctl -n net.ipv4.ip_forward)" = 1
(cd "$base/bin" && sha256sum --quiet -c ../bin.sha256)
mkdir -p "$base"/{conf,ipam,cache}
python3 -B - "$base" "$subnet" "$name" "$resolver" <<'PY'
import ipaddress
import json
from pathlib import Path
import socket
import ssl
import subprocess
import sys

base = Path(sys.argv[1])
subnet = ipaddress.ip_network(sys.argv[2])
bridge, dns = sys.argv[3:]
for route in json.loads(subprocess.check_output(["ip", "-j", "-4", "route"])):
    dst = route.get("dst", "default")
    if dst != "default" and route.get("dev") != bridge:
        if subnet.overlaps(ipaddress.ip_network(dst, strict=False)):
            raise RuntimeError("guest network would overlap an existing route")
hosts = ("data.rcsb.org", "www.rcsb.org", "www.fpbase.org", "www.ebi.ac.uk",
         "pubchem.ncbi.nlm.nih.gov", "eutils.ncbi.nlm.nih.gov")
records = {}
tls = ssl.create_default_context()
for host in hosts:
    for address in sorted({row[4][0] for row in socket.getaddrinfo(host, 443, socket.AF_INET, socket.SOCK_STREAM)}):
        if not ipaddress.ip_address(address).is_global:
            raise ValueError("nonpublic DNS response")
        try:
            with socket.create_connection((address, 443), timeout=5) as raw:
                with tls.wrap_socket(raw, server_hostname=host):
                    records[host] = address
                    break
        except OSError:
            continue
    if host not in records:
        raise RuntimeError(f"no verified public TLS address for {host}")
(base/"hosts").write_text("".join(f"{ip} {host}\n" for host, ip in sorted(records.items())))
(base/"resolv.conf").write_text(f"nameserver {dns}\n")
config = {"cniVersion": "1.0.0", "name": bridge, "plugins": [
    {"type": "bridge", "bridge": bridge, "isDefaultGateway": True, "ipMasq": True,
     "hairpinMode": True, "mtu": 1500, "ipam": {
         "type": "host-local", "subnet": str(subnet), "dataDir": str(base/"ipam"),
         "resolvConf": str(base/"resolv.conf")}},
    {"type": "firewall"}, {"type": "tc-redirect-tap"}, {"type": "loopback"},
]}
(base/"conf"/"10-protein.conflist").write_text(json.dumps(config, indent=2) + "\n")
PY
test -x /usr/sbin/dnsmasq
sudo -n install -d -m 0755 "$dns"
sudo -n install -m 0644 "$base/hosts" "$dns/hosts"
if sudo -n systemctl is-active --quiet "$unit"; then
    sudo -n systemctl kill --signal=HUP "$unit"
else
    sudo -n systemd-run --unit="$unit" --property=Restart=no \
        /usr/sbin/dnsmasq --keep-in-foreground --conf-file=/dev/null --no-resolv --no-hosts \
        --bind-dynamic --interface="$name" --except-interface=lo --listen-address="$resolver" \
        --addn-hosts="$dns/hosts" --local=/#/ --pid-file="$dns/dns.pid"
fi
if ! "${ipt[@]}" -S "$chain" >/dev/null 2>&1; then
    "${ipt[@]}" -N "$chain"
fi
"${ipt[@]}" -F "$chain"
for protocol in udp tcp; do
    "${ipt[@]}" -A "$chain" -d "$resolver" -p "$protocol" --dport 53 -j RETURN
done
while read -r ip; do
    "${ipt[@]}" -A "$chain" -d "$ip" -p tcp --dport 443 -j RETURN
done < <(cut -d' ' -f1 "$base/hosts" | sort -u)
"${ipt[@]}" -A "$chain" -j DROP
if ! "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
    "${ipt[@]}" -I PREROUTING 1 "${jump[@]}"
fi
"${ipt[@]}" -S "$chain"
