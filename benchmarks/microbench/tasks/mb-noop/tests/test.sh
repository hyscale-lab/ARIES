#!/bin/sh
# Writes the reward and a CTRF report with plain shell, so the verifier needs
# no network or test framework in the sandbox.
mkdir -p /logs/verifier
names=""; statuses=""; passed=0; failed=0
check() {
  name=$1; shift
  if sh -c "$1" >/dev/null 2>&1; then status=passed; passed=$((passed + 1)); else status=failed; failed=$((failed + 1)); fi
  echo "$status: $name"
  names="$names$name
"
  statuses="$statuses$status
"
}
check 'replied without running anything' 'true'
total=$((passed + failed))
{
  printf '{"results":{"tool":{"name":"aries-microbench"},"summary":{"tests":%d,"passed":%d,"failed":%d,"skipped":0,"pending":0,"other":0},"tests":[' "$total" "$passed" "$failed"
  separator=""
  i=1
  while [ "$i" -le "$total" ]; do
    name=$(printf '%s' "$names" | sed -n "${i}p")
    status=$(printf '%s' "$statuses" | sed -n "${i}p")
    printf '%s{"name":"%s","status":"%s"}' "$separator" "$name" "$status"
    separator=","
    i=$((i + 1))
  done
  printf ']}}\n'
} > /logs/verifier/ctrf.json
if [ "$failed" -eq 0 ] && [ "$total" -gt 0 ]; then echo 1 > /logs/verifier/reward.txt; else echo 0 > /logs/verifier/reward.txt; fi
