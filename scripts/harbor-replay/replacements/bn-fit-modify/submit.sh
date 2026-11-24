#!/usr/bin/env bash
# Submit one sealed stage, then transfer and acknowledge every closed archive.
set -euo pipefail
node=$1
[[ $node =~ ^amd(182|191|198|200|201)$ ]]
assets=$(realpath "$2")
raw=$(realpath -m "$3")
host=nehalem@$node.utah.cloudlab.us
stage=/local/zb-bn-fit-modify-01
unit=zb-bn-fit-replay-01
ssh_run=(ssh -o BatchMode=yes -o ConnectTimeout=10)
umask 077
mkdir -p "$raw"
log=$raw/$node-submission.log
if ! "${ssh_run[@]}" -n "$host" "sudo -n test -s '$stage/ready'"; then
    "${ssh_run[@]}" -n "$host" 'rc=0; sudo -n pgrep -x "firecracker|[.]firecracker-re" >/dev/null || rc=$?; test "$rc" = 1'
    tar -czf - -C "$assets" . | "${ssh_run[@]}" "$host" "umask 077; mkdir '$stage' && tar -xzf - -C '$stage'"
    "${ssh_run[@]}" -n "$host" "set -e; cd '$stage'; sha256sum --quiet -c SHA256SUMS;
      sudo -n systemd-run --unit='$unit' --collect /bin/bash '$stage/queue.sh' '$stage'; printf 'sealed\n' >ready" >>"$log"
fi
"${ssh_run[@]}" -n "$host" "systemctl show '$unit' -p ActiveState -p SubState -p InvocationID" >>"$log"
printf '%s %s sealed\n' "$(date -u +%FT%TZ)" "$node" >>"$log"
while :; do
    ready=$("${ssh_run[@]}" -n "$host" "sudo -n find '$stage/runs' -maxdepth 2 -name closed -printf '%h\n'")
    while IFS= read -r out; do
        [[ -n $out ]] || continue
        key=${out##*/}
        [[ $key =~ ^[a-f0-9]{8}-v[1-4]-bn-fit-(ordinary|deep)-01$ ]]
        archive=$raw/$key-worker.tar.gz
        if [[ ! -f $archive ]]; then
            "${ssh_run[@]}" -n "$host" "sudo -n tar -czf - -C '$stage/runs' '$key'" >"$archive.partial"
            mv "$archive.partial" "$archive"
        fi
        "${ssh_run[@]}" -n "$host" "sudo -n test -f '$out/archived' || sudo -n touch '$out/archived'"
    done <<<"$ready"
    if "${ssh_run[@]}" -n "$host" "sudo -n test -f '$stage/controller.complete'"; then
        "${ssh_run[@]}" -n "$host" "sudo -n cat '$stage/queue.tsv'" >"$raw/$node-worker-terminal.tsv.partial"
        mv "$raw/$node-worker-terminal.tsv.partial" "$raw/$node-worker-terminal.tsv"
        break
    fi
    "${ssh_run[@]}" -n "$host" "systemctl is-active --quiet '$unit' || sudo -n test -f '$stage/controller.complete'"
    sleep 12
done
