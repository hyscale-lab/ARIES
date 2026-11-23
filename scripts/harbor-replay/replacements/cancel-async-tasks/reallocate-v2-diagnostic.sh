#!/usr/bin/env bash
# Retire the deferred amd200 diagnostic driver before moving its only eligible cell.
set -euo pipefail
handoff=$(realpath "$1")
old=zb-cancel-async-diagnostic-amd200.service
invocation=$2
pid=$3
[[ "$invocation" =~ ^[a-f0-9]{32}$ && "$pid" =~ ^[0-9]+$ ]]
host=nehalem@amd200.utah.cloudlab.us
check_worker() {
    ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$host" '
       test ! -e /local/zb-cancel-async-02 &&
       test "$(systemctl show zb-cancel-async-diagnostic-02.service -p MainPID --value)" = 0
    '
}
test "$(systemctl show "$old" -p InvocationID --value)" = "$invocation"
test "$(systemctl show "$old" -p MainPID --value)" = "$pid"
test -z "$(find "$handoff" -mindepth 1 -maxdepth 1 -print -quit)"
check_worker
ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$host" 'systemctl is-active --quiet zb-sanitize-retry-02.service'
systemctl show "$old" -p ActiveState,SubState,InvocationID,MainPID,ExecStart > "$handoff/previous-unit.txt"
sudo -n systemctl stop "$old"
test "$(systemctl show "$old" -p MainPID --value)" = 0
test "$(systemctl show "$old" -p ActiveState --value)" = inactive
check_worker
sudo -n systemctl stop zb-cancel-async-collect-diagnostic-amd200.service
printf '%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$invocation" "$pid" > "$handoff/retired-invocation.tsv"
cat > "$handoff/acknowledge.sh" <<'ACK'
#!/usr/bin/env bash
set -eu
stage=$1
test -s "$stage/retired-invocation.tsv"
( set -o noclobber
  printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$INVOCATION_ID" "f265ccca-v2-cancel-async-ordinary-02" \
     "amd200_skip_no_dispatch_assign_amd183" > "$stage/skip-ack.tsv"
)
ACK
sudo -n systemd-run --unit="$old" --uid="$(id -un)" --working-directory="$handoff" \
    /usr/bin/bash "$handoff/acknowledge.sh" "$handoff"
for i in 1 2 3 4 5; do
    if [[ -s $handoff/skip-ack.tsv ]]; then break; fi
    sleep 1
done
test "$(cut -f3 "$handoff/skip-ack.tsv")" = f265ccca-v2-cancel-async-ordinary-02
test "$(cut -f4 "$handoff/skip-ack.tsv")" = amd200_skip_no_dispatch_assign_amd183
check_worker
cat "$handoff/retired-invocation.tsv" "$handoff/skip-ack.tsv"
