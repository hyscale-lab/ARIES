#!/usr/bin/env bash
set -euo pipefail
stage=$(realpath "$1")
node=$(cat /var/emulab/boot/nodeid)
case "$node" in
  amd182) modes=(v1:ordinary); tools=/local/zb-validation/replay/codex-harbor-tools.tar.gz ;;
  amd200) modes=(v2:ordinary); tools=/local/zb-corpus-financeapt-cohort-v2/codex-harbor-tools.tar.gz ;;
  amd191) modes=(v3:ordinary); tools=/local/zb-fc-v4/pilot/pypi-cohort-amd191/codex-harbor-tools.tar.gz ;;
  amd198) modes=(v4:ordinary); tools=/local/zb-imagecircuit-v4/video-assigned-assets/codex-harbor-tools.tar.gz ;;
  amd201) modes=(v1:deep v4:deep); tools=/local/zb-corpus-image-circuit-cohort-v3/codex-harbor-tools.tar.gz ;;
  *) exit 2 ;;
esac
umask 077
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
for arm in "${modes[@]}"; do
    IFS=: read -r variant mode <<<"$arm"
    case "$node:$variant" in
      amd182:v1) unit=fcmod-zepbound-amd182; runtime=/local/zb-validation/runtime ;;
      amd200:v2) unit=fcmod-zepbound-amd200; runtime=/local/zb-v2/runtime ;;
      amd191:v3) unit=fcmod-zepbound-amd191-v3; runtime=/local/zb-fc-v4/v3/runtime ;;
      amd198:v4) unit=fcmod-zepbound-amd198-imagecircuit; runtime=/local/zb-imagecircuit-v4/runtime ;;
      amd201:v1) unit=fcmod-zepbound-cancel-async-deep-v1; runtime=/local/zb-cancel-async-01/deep-v1/runtime ;;
      amd201:v4) unit=fcmod-zepbound-cancel-async-deep-v4; runtime=/local/zb-cancel-async-deep-v4-amd201/runtime ;;
    esac
    trials=$(python3 -B - "$stage/source.json" "$mode" <<'PY'
import json,sys
r=json.load(open(sys.argv[1])); median=r["median_source"]
ids=[median] if sys.argv[2]=="deep" else [median]+[x["trial"] for x in r["reviewed"]["trials"] if x["trial"]!=median]
print("\n".join(x[:8] for x in ids))
PY
)
    while IFS= read -r trial; do
        key=$trial-$variant-bn-fit-$mode-01
        out=$stage/runs/$key
        test ! -e "$out"
        status=0
        /bin/bash "$stage/run-worker.sh" "$stage" "$trial" "$variant" "$mode" "$runtime" "$unit" "$tools" \
            >"$stage/$key-controller.log" 2>&1 || status=$?
        printf '%s\t%d\n' "$key" "$status" >>"$stage/queue.tsv"
        test -d "$out"
        touch "$out/closed"
        while [[ ! -e $out/archived ]]; do sleep 4; done
        # A preflight failure may apply to every queued source on this lane.
        test -s "$out/result/events.jsonl"
        test -s "$out/idle-after.json"
    done <<<"$trials"
done
touch "$stage/controller.complete"
