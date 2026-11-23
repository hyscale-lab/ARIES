#!/usr/bin/env bash
# Execute from the runner, waiting for the entire preceding private queue.
set -euo pipefail
variant="$1"
root=$(realpath "$2")
base=/local/zb-relaxed-compcert-01
case "$variant" in
    v3) node=amd189; prior=v2 ;;
    v4) node=amd182; prior=v1 ;;
    *) exit 2 ;;
esac
host="nehalem@$node.utah.cloudlab.us"
while :; do
    state=$(ssh -o ConnectTimeout=10 "$host" "systemctl show -p ActiveState --value zb-relaxed-compcert-$prior") || { sleep 30; continue; }
    case "$state" in
        active|activating|deactivating) sleep 45 ;;
        inactive) break ;;
        *) printf 'Predecessor state %s\n' "$state" >&2; exit 1 ;;
    esac
done
ssh "$host" "sudo -n bash -c 'set -eu; test \$(wc -l < $base/$prior/series-status.txt) = 5; test \$(systemctl show -p ExecMainStatus --value zb-relaxed-compcert-$prior) = 0; ! ps -eo comm= | grep -Eq \"^firecracker$|^[.]firecracker-re$\"'"
"$root/repo/ARIES/scripts/relaxed-compcert/collect-series.sh" "$node" "$prior" "$root"
if [[ $variant == v3 ]]; then
    unit=fcmod-zepbound-amd189-stockv3
    rt=/local/zb-stock189-v3/runtime
    plugins=/local/zb-compcert-network/opam-cni
    tools=/local/zb-compcert-network/replay/codex-harbor-tools.tar.gz
else
    unit=fcmod-zepbound-relaxed-cc-v4-182
    stack=/local/zb-relaxed-compcert-v4-runtime
    rt=$stack/runtime
    plugins=/local/zb-deep-kv-v1-amd182/cni
    tools=/local/zb-torch-deep-v1-amd182/assets/codex-harbor-tools.tar.gz
    provision="$root/repo/ARIES/scripts/zb-trace/provision_baseline.sh"
    profile="$root/repo/firecracker-containerd/.git/zepbound-amd189/02a77c58-v4-04/profile.json"
    binaries="$root/repo/ARIES/.cache/zb-raw/optimized-bin.tar.gz"
    ssh "$host" "sudo -n mkdir -p $rt; sudo -n test ! -e $rt/runtime.json"
    ssh "$host" "sudo -n tar -C $rt -xzf -" < "$binaries"
    ssh "$host" "sudo -n tee $stack/provision.sh >/dev/null" < "$provision"
    ssh "$host" "sudo -n tee $stack/donor-profile.json >/dev/null" < "$profile"
    ssh "$host" "sudo -n bash -c 'set -eu; donor=/local/zb-validation/runtime; cp --reflink=auto \$donor/common-{rootfs.ext4,vmlinux.bin,vmlinux.config} \$donor/jailer.json $rt/; sha256sum $rt/bin/* $rt/common-* $stack/{provision.sh,donor-profile.json} > $stack/deployment-inputs.sha256; bash $stack/provision.sh $stack $unit $stack/donor-profile.json v4 docker.io/alexgshaw/compile-compcert@sha256:f60236a4fe55bbb3c573210fe0fcd91551f4ee97c417453cd4985be074b35411 > $stack/provision.log 2>&1'"
fi
ssh "$host" "sudo -n systemd-run --unit=zb-relaxed-compcert-$variant $base/run-series.sh $base $variant $unit $rt $plugins $tools 1d11 d116 dc2b bf3c 921a"
