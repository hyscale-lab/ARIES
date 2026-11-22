#!/usr/bin/env bash
# Configure a private validation bridge with egress for its reviewed dependencies.
set -euo pipefail
base=$(realpath "$1")
action=${2:-install}
bridge=zbvid183
subnet=10.231.183.0/28
chain=ZB183PYPI
name=zbvideo183
tls_hosts=(pypi.org files.pythonhosted.org)
http_hosts=()
github_addresses=()
case "${3:-video}" in
    video) ;;
    compcert)
        bridge=zbcc189
        subnet=10.231.189.0/28
        chain=ZB189COMP
        name=zbcomp189
        tls_hosts=(github.com)
        # The host and guest resolver return different regional frontends.
        # Require an authenticated response for this exact public repository
        # from each additional address before allowing it on this bridge.
        for octet in 112 113 114; do
            for last in 3 4; do
                candidate=140.82.$octet.$last
                if curl --noproxy '*' --fail --silent --show-error --max-time 6 \
                    --resolve "github.com:443:$candidate" --output /dev/null \
                    'https://github.com/AbsInt/CompCert.git/info/refs?service=git-upload-pack'; then
                    github_addresses+=("$candidate")
                fi
            done
        done
        http_hosts=(archive.ubuntu.com security.ubuntu.com)
        ;;
    *) echo "Unknown validation network profile" >&2; exit 2 ;;
esac
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
(base/"conf"/"10-video.conflist").write_text(json.dumps(config,indent=2)+"\n")
PY
if ! "${ipt[@]}" -S "$chain" &>/dev/null; then
    "${ipt[@]}" -N "$chain"
    for protocol in udp tcp; do "${ipt[@]}" -A "$chain" -d "$resolver" -p "$protocol" --dport 53 -j RETURN; done
    mapfile -t addresses < <(getent ahostsv4 "${tls_hosts[@]}" | awk '{print $1}' | sort -u)
    (("${#addresses[@]}" > 0))
    for address in "${addresses[@]}"; do "${ipt[@]}" -A "$chain" -d "$address" -p tcp --dport 443 -j RETURN; done
    if (("${#http_hosts[@]}" > 0)); then
        mapfile -t archives < <(getent ahostsv4 "${http_hosts[@]}" | awk '{print $1}' | sort -u)
        (("${#archives[@]}" > 0))
        for address in "${archives[@]}"; do "${ipt[@]}" -A "$chain" -d "$address" -p tcp --dport 80 -j RETURN; done
    fi
    "${ipt[@]}" -A "$chain" -j DROP
fi
for address in "${github_addresses[@]}"; do
    if ! "${ipt[@]}" -C "$chain" -d "$address" -p tcp --dport 443 -j RETURN 2>/dev/null; then
        "${ipt[@]}" -I "$chain" 1 -d "$address" -p tcp --dport 443 -j RETURN
    fi
done
if ! "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
    "${ipt[@]}" -I PREROUTING 1 "${jump[@]}"
fi
"${ipt[@]}" -S "$chain"
