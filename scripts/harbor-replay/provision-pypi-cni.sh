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
private_dns=false
pin_egress=false
dns_unit=
case "${3:-video}" in
    video|video-amd182|video-amd183|video-amd189|video-amd191|video-amd198|video-amd200|video-amd201|video-amd203)
        node=183
        if [[ ${3:-video} == video-amd* ]]; then node=${3#video-amd}; fi
        bridge=zbvid$node
        if [[ $node != 183 ]]; then subnet=10.234.$node.0/28; fi
        # The financial-document deployment on amd182 owns 10.234.182.0/28.
        if [[ $node == 182 ]]; then subnet=10.240.182.0/28; fi
        chain=ZB${node}PYPI
        name=zbvideo$node
        ;;
    dataset|dataset-amd182|dataset-amd183|dataset-amd189|dataset-amd191|dataset-amd198|dataset-amd200|dataset-amd201|dataset-amd203)
        private_dns=true
        node=189
        if [[ ${3:-video} == dataset-amd* ]]; then node=${3#dataset-amd}; fi
        bridge=zbtok$node
        subnet=10.232.$node.0/28
        # The amd182 source-validation inference bridge owns 10.232.182.0/28.
        if [[ "$node" == 182 ]]; then subnet=10.238.182.0/28; fi
        chain=ZB${node}TOK
        name=zbtoken$node
        dns_unit=zb-token-dns-amd$node
        tls_hosts=(huggingface.co us.aws.cdn.hf.co pypi.org files.pythonhosted.org)
        ;;
    inference-amd182|hf-amd182|hf-amd183|hf-amd189|hf-amd191|hf-amd198|hf-amd200|hf-amd201|hf-amd203)
        private_dns=true
        node=${3##*amd}
        bridge=zbhfm$node
        subnet=10.235.$node.0/28
        chain=ZB${node}HF235
        name=zbhfm$node
        dns_unit=zb-hf-dns-amd$node
        if [[ $node == 189 ]]; then
            subnet=10.237.189.0/28
            chain=ZB189HF237
        fi
        if [[ $3 == inference-amd182 ]]; then
            bridge=zbhf182
            subnet=10.232.182.0/28
            chain=ZB182HF
            name=zbinference182
            dns_unit=zb-inference-dns-amd182
        fi
        tls_hosts=(huggingface.co us.aws.cdn.hf.co cas-server.xethub.hf.co transfer.xethub.hf.co pypi.org files.pythonhosted.org)
        ;;
    compcert-opam|compcert-opam-amd182|compcert-opam-amd183|compcert-opam-amd189|compcert-opam-amd191|compcert-opam-amd198|compcert-opam-amd200|compcert-opam-amd201|compcert-opam-amd203)
        private_dns=true
        node=189
        if [[ ${3:-video} == compcert-opam-amd* ]]; then node=${3#compcert-opam-amd}; fi
        bridge=zbopam$node
        subnet=10.233.$node.0/28
        chain=ZB${node}OPAM
        name=zbopam$node
        dns_unit=zb-opam-dns-amd$node
        tls_hosts=(github.com raw.githubusercontent.com opam.ocaml.org archive.ubuntu.com security.ubuntu.com)
        http_hosts=(archive.ubuntu.com security.ubuntu.com)
        ;;
    hof-systre-amd183)
        private_dns=true
        bridge=zbhof183
        subnet=10.247.183.0/28
        chain=ZB183HOF
        name=zbhof183
        dns_unit=zb-hof-dns-amd183
        tls_hosts=(api.github.com github.com raw.githubusercontent.com release-assets.githubusercontent.com snapshot.ubuntu.com)
        ;;
    shard-wave3-amd183)
        private_dns=true
        pin_egress=true
        bridge=zbshard3183
        subnet=10.251.183.0/28
        chain=ZB183SHARD3
        name=zbshard3183
        dns_unit=zb-shard3-dns-amd183
        tls_hosts=(raw.githubusercontent.com s3.amazonaws.com www.gutenberg.org)
        ;;
    compiler-snapshot-amd183)
        private_dns=true
        bridge=zbccsnap183
        subnet=10.239.183.0/28
        chain=ZB183CCSNAP
        name=zbccsnap183
        dns_unit=zb-ccsnap-dns-amd183
        tls_hosts=(github.com raw.githubusercontent.com opam.ocaml.org snapshot.ubuntu.com)
        ;;
    nextjs-be1d1d11-amd191)
        private_dns=true
        pin_egress=true
        bridge=zbnextbe1d191
        subnet=10.250.191.0/28
        chain=ZB191NEXTBE1D
        name=zbnext-be1d1d11-amd191
        dns_unit=zb-next-be1d1d11-dns-amd191
        # The Playwright CDN redirects Chromium and FFmpeg to these exact hosts.
        tls_hosts=(registry.npmjs.org cdn.playwright.dev storage.googleapis.com
                   playwright.download.prss.microsoft.com snapshot.debian.org)
        ;;
    wildclaw-amd201)
        private_dns=true
        pin_egress=true
        bridge=zbwcb201
        subnet=10.246.201.0/28
        chain=ZB201WCB
        name=zbwcb201
        dns_unit=zb-wcb-dns-amd201
        tls_hosts=()
        if [[ "$action" != remove ]]; then
            mapfile -t tls_hosts < "$base/source-hosts.txt"
            (( ${#tls_hosts[@]} > 0 ))
            for host in "${tls_hosts[@]}"; do
                [[ "$host" =~ ^[a-zA-Z0-9-]+(\.[a-zA-Z0-9-]+)+$ ]]
            done
        fi
        ;;
    compcert)
        bridge=zbcc189
        subnet=10.231.189.0/28
        chain=ZB189COMP
        name=zbcomp189
        tls_hosts=(github.com)
        http_hosts=(archive.ubuntu.com security.ubuntu.com)
        ;;
    *) echo "Unknown validation network profile" >&2; exit 2 ;;
esac
ipt=(sudo -n iptables -w -t mangle)
jump=(-i "$bridge" -s "$subnet" -m comment --comment "$name" -j "$chain")
if [[ $pin_egress == true ]]; then
    exec 9>"$base/nextjs-network.lock"
    flock 9
    jump=(-i "$bridge" -m comment --comment "$name" -j "$chain")
    ip6t=(sudo -n ip6tables -w -t mangle)
    deny_v6=(-i "$bridge" -m comment --comment "$name" -j DROP)
fi
if [[ "$action" == remove ]]; then
    if [[ $private_dns == true ]] && sudo -n systemctl is-active --quiet "$dns_unit"; then
        sudo -n systemctl stop "$dns_unit"
    fi
    if "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
        "${ipt[@]}" -D PREROUTING "${jump[@]}"
        "${ipt[@]}" -F "$chain"
        "${ipt[@]}" -X "$chain"
    fi
    if [[ $pin_egress == true ]] && "${ip6t[@]}" -C PREROUTING "${deny_v6[@]}" 2>/dev/null; then
        "${ip6t[@]}" -D PREROUTING "${deny_v6[@]}"
    fi
    exit 0
fi
test "$action" = install
if [[ ${3:-video} == compcert ]]; then
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
fi
test "$(sysctl -n net.ipv4.ip_forward)" = 1
mkdir -p "$base"/{conf,ipam,cache}
(cd "$base/bin"; sha256sum -c ../SHA256SUMS)
resolver=$(awk '$1=="nameserver" && $2 !~ /^127\./ && $2 !~ /:/ {print $2;exit}' /run/systemd/resolve/resolv.conf)
test -n "$resolver"
if [[ $private_dns == true ]]; then resolver=${subnet%.0/28}.1; fi
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
if [[ $private_dns == true ]]; then
    test -x /usr/sbin/dnsmasq
    python3 - "$base/conf/dataset-hosts.json" "${ZB_TLS_CANDIDATES:-}" "${tls_hosts[@]}" <<'PYTLS'
import ipaddress,json,pathlib,socket,ssl,sys
path=pathlib.Path(sys.argv[1]); hosts={}
candidates=json.loads(pathlib.Path(sys.argv[2]).read_text()) if sys.argv[2] else {}
for host in sys.argv[3:]:
    try:
        addresses=sorted({a[4][0] for a in socket.getaddrinfo(host,443,socket.AF_INET,socket.SOCK_STREAM)})
    except socket.gaierror:
        addresses=[]
    if host in candidates and candidates[host] not in addresses:
        addresses.append(candidates[host])
    for ip in addresses:
        if not ipaddress.ip_address(ip).is_global: raise ValueError(host)
        try:
            with socket.create_connection((ip,443),timeout=5) as conn:
                with ssl.create_default_context().wrap_socket(conn,server_hostname=host):
                    hosts[host]=ip
                    break
        except OSError:
            continue
    if host not in hosts: raise RuntimeError(f"no validated public TLS address for {host}")
path.write_text(json.dumps(hosts,sort_keys=True)+"\n")
(path.parent.parent/"dns.hosts").write_text("".join(f"{addr} {host}\n" for host, addr in sorted(hosts.items())))
PYTLS
    if [[ $pin_egress == true ]]; then
        # shortcut: enforce verified IPs and ports, use an application proxy if shared CDN addresses need hostname enforcement.
        # Replace the profile chain atomically so a DNS refresh retires old IPs.
        if ! "${ip6t[@]}" -C PREROUTING "${deny_v6[@]}" 2>/dev/null; then
            "${ip6t[@]}" -I PREROUTING 1 "${deny_v6[@]}"
        fi
        insert_jump=
        if ! "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
            insert_jump=true
        fi
        {
            printf '*mangle\n:%s - [0:0]\n' "$chain"
            printf -- '-A %s ! -s %s -j DROP\n' "$chain" "$subnet"
            for protocol in udp tcp; do
                printf -- '-A %s -d %s -p %s --dport 53 -j RETURN\n' "$chain" "$resolver" "$protocol"
            done
            if [[ ${3:-video} == wildclaw-amd201 && -S "$base/brave.sock" ]]; then
                printf -- '-A %s -d %s -p tcp --dport 443 -j RETURN\n' "$chain" "$resolver"
            fi
            while read -r address; do
                printf -- '-A %s -d %s -p tcp --dport 443 -j RETURN\n' "$chain" "$address"
            done < <(cut -d ' ' -f 1 "$base/dns.hosts" | sort -u)
            printf -- '-A %s -j DROP\n' "$chain"
            if [[ $insert_jump == true ]]; then
                printf -- '-I PREROUTING 1 %s\n' "${jump[*]}"
            fi
            printf 'COMMIT\n'
        } | sudo -n iptables-restore --wait --noflush
    fi
    # The platform mounts /etc/hosts read-only. Resolve service records on this
    # dedicated bridge and return NXDOMAIN for names outside the approved set.
    # The private runtime can be mode 0700. dnsmasq reloads records after
    # dropping privileges, so expose only the selected public DNS records.
    dns_runtime=/run/$dns_unit
    sudo -n install -d -m 0755 "$dns_runtime"
    sudo -n install -m 0644 "$base/dns.hosts" "$dns_runtime/hosts"
    if sudo -n systemctl is-active --quiet "$dns_unit" &&
       ! sudo -n systemctl show -p ExecStart --value "$dns_unit" | grep -Fq -- "--addn-hosts=$dns_runtime/hosts"; then
        sudo -n systemctl stop "$dns_unit"
    fi
    if sudo -n systemctl is-active --quiet "$dns_unit"; then
        sudo -n systemctl kill --signal=HUP "$dns_unit"
    else
        sudo -n systemd-run --unit="$dns_unit" --property=Restart=no \
            /usr/sbin/dnsmasq --keep-in-foreground --conf-file=/dev/null --no-hosts --no-resolv \
            --bind-dynamic --interface="$bridge" --except-interface=lo --listen-address="$resolver" \
            --addn-hosts="$dns_runtime/hosts" --local=/#/ --pid-file="$dns_runtime/dns.pid"
    fi
fi
if [[ $pin_egress == true ]]; then
    "${ipt[@]}" -C PREROUTING "${jump[@]}"
    "${ip6t[@]}" -C PREROUTING "${deny_v6[@]}"
    "${ipt[@]}" -S "$chain"
    exit 0
fi
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
if [[ $private_dns == true ]]; then
    mapfile -t verified < <(python3 -c 'import json,sys;print("\n".join(sorted(set(json.load(open(sys.argv[1])).values()))))' "$base/conf/dataset-hosts.json")
else
    verified=("${github_addresses[@]}")
fi
if [[ $private_dns == true ]]; then
    for protocol in udp tcp; do
        if ! "${ipt[@]}" -C "$chain" -d "$resolver" -p "$protocol" --dport 53 -j RETURN 2>/dev/null; then
            "${ipt[@]}" -I "$chain" 1 -d "$resolver" -p "$protocol" --dport 53 -j RETURN
        fi
    done
fi
if [[ ${3:-video} == compcert-opam* ]]; then
    # Ubuntu APT uses HTTP and verifies signed indices. Use exactly the
    # archive addresses the scoped resolver returns to this private guest.
    mapfile -t archives < <(python3 -c 'import json,sys;h=json.load(open(sys.argv[1]));print(h["archive.ubuntu.com"]);print(h["security.ubuntu.com"])' "$base/conf/dataset-hosts.json")
    for address in "${archives[@]}"; do
        if ! "${ipt[@]}" -C "$chain" -d "$address" -p tcp --dport 80 -j RETURN 2>/dev/null; then
            "${ipt[@]}" -I "$chain" 1 -d "$address" -p tcp --dport 80 -j RETURN
        fi
    done
fi
for address in "${verified[@]}"; do
    if ! "${ipt[@]}" -C "$chain" -d "$address" -p tcp --dport 443 -j RETURN 2>/dev/null; then
        "${ipt[@]}" -I "$chain" 1 -d "$address" -p tcp --dport 443 -j RETURN
    fi
done
if ! "${ipt[@]}" -C PREROUTING "${jump[@]}" 2>/dev/null; then
    "${ipt[@]}" -I PREROUTING 1 "${jump[@]}"
fi
"${ipt[@]}" -S "$chain"
