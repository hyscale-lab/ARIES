#!/usr/bin/env bash
# Prepare a dedicated existing runtime while it is idle. Guest execution is separate.
set -euo pipefail
stage=$(realpath "$1")
runtime=$(realpath "$2")
unit=$3
cni=$4
variant=$5
node=$6
image=$7
cd "$stage"
[[ $variant =~ ^v[1-4]$ && $node =~ ^amd(182|189|198|200)$ && $unit =~ ^fcmod-zepbound-[a-z0-9-]+$ ]]
[[ $image == docker.io/alexgshaw/video-processing@sha256:a2c0f39e3ab04e67ac6e49cad9167bb0d987babe32511768c01aa0c6905a875f ]]
if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 3; fi
systemctl is-active "$unit"
if [[ ! -f "$cni/conf/10-video.conflist" ]]; then
    [[ "$node" == amd182 && "$cni" == "$stage/cni" ]]
    bash provision-pypi-cni.sh "$cni" install video-amd182 > cni-install.log
fi
(cd "$cni/bin" && sha256sum --quiet -c ../SHA256SUMS)
sudo -n python3 - "$cni" "$node" > network.json <<'PY'
import hashlib,ipaddress,json,socket,ssl,sys
from pathlib import Path
p=Path(sys.argv[1]); node=sys.argv[2]
c=json.loads((p/"conf/10-video.conflist").read_text())
assert c["name"]=="zbvideo"+node[3:]
assert hashlib.sha256((p/"SHA256SUMS").read_bytes()).hexdigest()=="8479939145e56bf6480b96f99baeb0b586d039d13ba047e928b692462b0fece0"
e={}
for host in ("pypi.org","files.pythonhosted.org"):
    with socket.create_connection((host,443),timeout=10) as raw, ssl.create_default_context().wrap_socket(raw,server_hostname=host) as tls:
        e[host]={"address":tls.getpeername()[0],"protocol":tls.version(),"certificate_sha256":hashlib.sha256(tls.getpeercert(binary_form=True)).hexdigest()}
        assert ipaddress.ip_address(e[host]["address"]).is_global
print(json.dumps({"cni":c,"tls":e,"plugins_sha256":hashlib.sha256((p/"SHA256SUMS").read_bytes()).hexdigest()},indent=2))
PY
sudo -n python3 audit-idle-runtime.py "$unit" "$runtime" > deployment-readback.json
sudo -n cat "$runtime/runtime.json" > effective-profile.json
sudo -n "$runtime/bin/firecracker-ctr" --address "/run/$unit/containerd.sock" --namespace aries images pull --local --platform linux/amd64 --snapshotter devmapper "$image" > image-pull.log
sudo -n "$runtime/bin/firecracker-ctr" --address "/run/$unit/containerd.sock" --namespace aries images ls -q | grep -Fx "$image" > task-image.txt
bpftrace --version > bpftrace-version.txt
if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 3; fi
