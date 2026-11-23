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
    cleanse)
        subnet=10.248.$octet.0/28
        hosts=(deb.debian.org security.debian.org pypi.org files.pythonhosted.org github.com api.github.com release-assets.githubusercontent.com objects.githubusercontent.com)
        ;;
    sparql)
        subnet=10.231.$octet.0/28
        hosts=(bootstrap.pypa.io pypi.org files.pythonhosted.org raw.githubusercontent.com)
        ;;
    torch)
        subnet=10.236.$octet.0/28
        hosts=(bootstrap.pypa.io pypi.org files.pythonhosted.org download.pytorch.org download-r2.pytorch.org)
        ;;
    financeapt)
        subnet=10.234.$octet.0/28
        if [[ "$worker" == amd201 ]]; then subnet=10.237.$octet.0/28; fi
        hosts=(snapshot.ubuntu.com pypi.org files.pythonhosted.org)
        ;;
    pypi)
        subnet=10.233.$octet.0/28
        if [[ "$worker" == amd183 ]]; then subnet=10.236.183.0/28; fi
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
    for address in "${addresses[@]}"; do "${ipt[@]}" -A "$chain" -d "$address" -p tcp -m multiport --dports 80,443 -j RETURN; done
    "${ipt[@]}" -A "$chain" -j DROP
fi
# Worker and guest resolvers can return different GitHub frontends. Add only
# endpoints which authenticate the source repository over TLS on this worker.
if [[ "$profile" == cleanse ]]; then
    for host in github.com api.github.com; do
        if [[ "$host" == github.com ]]; then
            tails=(3 4)
            resource=/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz
        else
            tails=(5 6)
            resource=/repos/gitleaks/gitleaks/releases/tags/v8.30.1
        fi
        for region in 112 113 114; do
            for last in "${tails[@]}"; do
                addr=140.82.$region.$last
                if curl --noproxy '*' --fail --silent --head --max-time 6 --resolve "$host:443:$addr" "https://$host$resource" --output /dev/null; then
                    rule=(-d "$addr" -p tcp --dport 443 -j RETURN)
                    if ! "${ipt[@]}" -C "$chain" "${rule[@]}" 2>/dev/null; then
                        "${ipt[@]}" -I "$chain" 1 "${rule[@]}"
                    fi
                fi
            done
        done
    done
fi
if ! "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
    "${ipt[@]}" -I PREROUTING 1 "${jump[@]}"
fi
"${ipt[@]}" -S "$chain"
