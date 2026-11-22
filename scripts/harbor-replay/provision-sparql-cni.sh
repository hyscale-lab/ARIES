#!/usr/bin/env bash
# Configure a private source dependency network on one study worker.
set -euo pipefail
base=$(realpath "$1")
action=${2:-install}
# CloudLab can assign a short hostname unrelated to the physical worker label.
worker=${3:-amd198}
profile=${4:-sparql}
[[ "$worker" =~ ^amd([1-9][0-9]{1,2})$ ]]
octet=${BASH_REMATCH[1]}
((octet <= 254))
case "$profile" in
    sparql)
        subnet=10.231.$octet.0/28
        hosts=(bootstrap.pypa.io pypi.org files.pythonhosted.org raw.githubusercontent.com)
        ;;
    torch)
        subnet=10.232.$octet.0/28
        hosts=(bootstrap.pypa.io pypi.org files.pythonhosted.org download.pytorch.org download-r2.pytorch.org)
        ;;
    financeapt)
        subnet=10.234.$octet.0/28
        hosts=(snapshot.ubuntu.com pypi.org files.pythonhosted.org)
        ;;
    pypi)
        subnet=10.233.$octet.0/28
        hosts=(pypi.org files.pythonhosted.org)
        ;;
    *) exit 2 ;;
esac
name=zb$profile$octet
bridge=$name
chain=ZB$octet${profile^^}
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
python3 - "$base" "$subnet" "$bridge" "$name" "$profile" <<'PY'
import ipaddress, json, pathlib, subprocess, sys
base=pathlib.Path(sys.argv[1]); subnet=ipaddress.ip_network(sys.argv[2])
bridge,name,profile=sys.argv[3:]
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
(base/"conf"/f"10-{profile}.conflist").write_text(json.dumps(config,indent=2)+"\n")
PY
if ! "${ipt[@]}" -S "$chain" &>/dev/null; then
    "${ipt[@]}" -N "$chain"
    for protocol in udp tcp; do "${ipt[@]}" -A "$chain" -d "$resolver" -p "$protocol" --dport 53 -j RETURN; done
    mapfile -t addresses < <(getent ahostsv4 "${hosts[@]}" | awk '{print $1}' | sort -u)
    (("${#addresses[@]}" > 0))
    for address in "${addresses[@]}"; do "${ipt[@]}" -A "$chain" -d "$address" -p tcp --dport 443 -j RETURN; done
    "${ipt[@]}" -A "$chain" -j DROP
fi
if ! "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
    "${ipt[@]}" -I PREROUTING 1 "${jump[@]}"
fi
"${ipt[@]}" -S "$chain"
