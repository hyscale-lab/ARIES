#!/usr/bin/env bash
# Called only after the original and prerequisite worker queues have closed.
set -euo pipefail
stage=$(realpath "$1")
old=$(realpath "$2")
node=$(cat /var/emulab/boot/nodeid)
case "$node" in
  amd203) modes=(v1:ordinary v1:deep) ;;
  amd191) modes=(v3:ordinary) ;;
  amd198) modes=(v4:deep v4:ordinary) ;;
  amd200) modes=(v2:ordinary) ;;
  *) exit 2 ;;
esac
umask 077
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
for arm in "${modes[@]}"; do
  IFS=: read -r variant mode <<<"$arm"
  trials=(6d0403e0)
  if [[ $mode == ordinary ]]; then
    trials+=(1b1460f4 5854a3a5 a841b950 e9635d5f)
  fi
  for trial in "${trials[@]}"; do
    oldkey=$trial-$variant
    if [[ $mode == deep ]]; then oldkey=$trial-deep-$variant; fi
    newkey=$trial-$variant-sanitize-retry-$mode-02
    if python3 -B "$stage/retained-original.py" "$old/results/$oldkey" "$stage/expected-calls.json" "$trial"; then
      printf '%s\t%s\n' "$newkey" "$oldkey" >>"$stage/retained-originals.tsv"
      continue
    fi
    test ! -e "$stage/runs/$newkey"
    status=0
    bash "$stage/run-retry.sh" "$stage" "$old" "$trial" "$variant" "$mode" \
        >"$stage/$newkey-controller.log" 2>&1 || status=$?
    printf '%s\n' "$status" >"$stage/runs/$newkey/controller.exit"
    touch "$stage/runs/$newkey/closed"
    while [[ ! -e "$stage/runs/$newkey/archived" ]]; do sleep 5; done
  done
done
touch "$stage/controller.complete"
