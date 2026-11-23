#!/usr/bin/env bash
# Serial dispatcher, after the assigned HF tail is terminal and no VMM survives.
set -euo pipefail
stage=$(realpath "$1")
node=$(cat /var/emulab/boot/nodeid)
case "$node" in
    amd183)
        gate=v2
        donor=/local/zb-video-network/cni/bin
        arms=(v1:ordinary v2:ordinary v1:deep)
        ;;
    amd198)
        gate=v4
        donor=/local/zb-sparql/cni/bin
        arms=(v3:ordinary v4:ordinary v4:deep)
        ;;
    *) exit 2 ;;
esac
sudo -n test -s "/tmp/zb-timing17-01/b615a3ec-$gate/driver.exit"
test "$(systemctl show -p ActiveState --value zb-timing17-hf.service)" = inactive
! sudo -n pgrep -x 'firecracker|[.]firecracker-re'
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
cni=$stage/cni
mkdir -p "$cni/bin"
while read -r _ name; do
    install -m 0755 "$donor/$name" "$cni/bin/$name"
done < "$stage/cni-bin.sha256"
cp "$stage/cni-bin.sha256" "$cni/bin.sha256"
bash "$stage/network.sh" "$cni" install "${node#amd}" > "$stage/network-setup.log"
trap 'bash "$stage/network.sh" "$cni" remove "${node#amd}"' EXIT
for arm in "${arms[@]}"; do
    status=0
    bash "$stage/worker.sh" "$stage" "${arm%:*}" "${arm#*:}" || status=$?
    printf '%s\t%s\n' "$arm" "$status" >> "$stage/queue.tsv"
done
