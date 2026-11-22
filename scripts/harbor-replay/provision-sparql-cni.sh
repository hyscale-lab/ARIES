#!/usr/bin/env bash
# Configure only the amd198 private SPARQL validation bridge and its dependency egress.
set -euo pipefail
base=$(realpath "$1")
action=${2:-install}
bridge=zbsparql198
subnet=10.231.198.0/28
chain=ZB198SPARQL
name=zbsparql198
ipt=(sudo -n iptables -w -t mangle)
jump=(-i "$bridge" -s "$subnet" -m comment --comment "$name" -j "$chain")
if [[ "$action" == remove ]]; then
    if "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
        "${ipt[@]}" -D PREROUTING "${jump[@]}"
        "${ipt[@]}" -F "$chain"
        "${ipt[@]}" -X "$chain"
    fi
    exit 0
fi
test "$action" = install
test "$(sysctl -n net.ipv4.ip_forward)" = 1
mkdir -p "$base"/{conf,ipam,cache}
(cd "$base/bin"; sha256sum -c ../SHA256SUMS)
resolver=$(awk '$1=="nameserver" && $2 !~ /^127\./ && $2 !~ /:/ {print $2;exit}' /run/systemd/resolve/resolv.conf)
test -n "$resolver"
printf 'nameserver %s\n' "$resolver" > "$base/resolv.conf"
python3 - "$base" "$subnet" "$bridge" "$name" <<'PY'
import ipaddress, json, pathlib, subprocess, sys
base=pathlib.Path(sys.argv[1]); subnet=ipaddress.ip_network(sys.argv[2])
bridge,name=sys.argv[3:]
routes=json.loads(subprocess.check_output(["ip","-j","-4","route"]))
for route in routes:
    dst=route.get("dst","default")
    if dst=="default" or route.get("dev")==bridge:
        continue
    if subnet.overlaps(ipaddress.ip_network(dst,strict=False)):
        raise RuntimeError(f"candidate network overlaps {route}")
config={"cniVersion":"1.0.0","name":name,"plugins":[{"type":"bridge","bridge":bridge,
    "isDefaultGateway":True,"forceAddress":False,"ipMasq":True,"hairpinMode":True,"mtu":1500,
    "ipam":{"type":"host-local","subnet":str(subnet),"dataDir":str(base/"ipam"),
            "resolvConf":str(base/"resolv.conf")}},{"type":"firewall"},{"type":"tc-redirect-tap"},{"type":"loopback"}]}
(base/"conf"/"10-sparql.conflist").write_text(json.dumps(config,indent=2)+"\n")
PY
if ! "${ipt[@]}" -S "$chain" &>/dev/null; then
    "${ipt[@]}" -N "$chain"
    for protocol in udp tcp; do "${ipt[@]}" -A "$chain" -d "$resolver" -p "$protocol" --dport 53 -j RETURN; done
    mapfile -t addresses < <(getent ahostsv4 bootstrap.pypa.io pypi.org files.pythonhosted.org raw.githubusercontent.com | awk '{print $1}' | sort -u)
    (("${#addresses[@]}" > 0))
    for address in "${addresses[@]}"; do "${ipt[@]}" -A "$chain" -d "$address" -p tcp --dport 443 -j RETURN; done
    "${ipt[@]}" -A "$chain" -j DROP
fi
if ! "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
    "${ipt[@]}" -I PREROUTING 1 "${jump[@]}"
fi
"${ipt[@]}" -S "$chain"
