#!/bin/bash
set -euo pipefail
ulimit -c 0
umask 077
base=/local/zb-study3-stock
cd "$base"
mode=${1:-default}
action=${2:-smoke}
[[ "$mode" =~ ^[a-zA-Z0-9_-]+$ ]]
[[ "$action" == smoke || "$action" == serve ]]
work="$base/$mode"
config="$base/server-bundle/default.toml"
if [[ "$mode" == pack* ]]; then
  config="$base/pack.toml"
fi
config=${3:-"$config"}
mkdir "$work"
mkdir "$work"/{home,run,tmp,xdg}
cp -R --preserve=mode,timestamps "$base/server-bundle/deps" "$base/server-bundle/ublk" "$work/home/"
if [[ ! -d /etc/overlaybd ]]; then
  mkdir "$work"/{etc-upper,etc-work}
  mount -t overlay overlay -o "lowerdir=/etc,upperdir=$work/etc-upper,workdir=$work/etc-work" /etc
  mkdir /etc/overlaybd
fi
mount --bind "$base/server-bundle/etc/overlaybd" /etc/overlaybd
export AENV_HOME_PATH="$work/home"
export AENV_RUNTIME_PATH="$work/run"
export API_ADDR=127.0.0.1:18763
export AENV_API_KEY
AENV_API_KEY=$(openssl rand -hex 24)
export TMPDIR="$work/tmp"
export XDG_CONFIG_HOME="$work/xdg"
export REGCTL_CONFIG="$base/registry/regctl.json"
export PATH="$base/os-packages/usr/bin:$PATH"
export RUST_LOG=info,agentenv::sandbox::firecracker::startup_pack=debug,agentenv::sandbox::firecracker::sandbox=debug
export AENV_LOG_FORMAT=json
aenv="$base/cli-bundle/aenv"
mkdir "$XDG_CONFIG_HOME/aenv"
printf '%s\n' 'url = "http://127.0.0.1:18763"' "api_key = \"$AENV_API_KEY\"" >"$XDG_CONFIG_HOME/aenv/credentials"
chmod 600 "$XDG_CONFIG_HOME/aenv/credentials"
ip link set lo up
sysctl -qw net.ipv4.ip_forward=1
"$base/os-packages/usr/bin/docker-registry" serve "$base/registry/config.yml" >"$work/registry.log" 2>&1 &
registry_pid=$!
server_pid=
cleanup() {
  local code=$?
  trap - EXIT
  if [[ -n "$server_pid" ]]; then
    kill -TERM "$server_pid" 2>/dev/null || true
    wait "$server_pid" || true
  fi
  kill -TERM "$registry_pid" 2>/dev/null || true
  wait "$registry_pid" || true
  printf 'exit=%s\n' "$code"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf 'start=%s\n' "$(date -u +%FT%T.%NZ)"
printf 'network=%s mount=%s config=%s\n' "$(readlink /proc/self/ns/net)" "$(readlink /proc/self/ns/mnt)" "$config"
"$base/server-bundle/server" --config "$config" >"$work/server.log" 2>&1 &
server_pid=$!
for _ in $(seq 45)
do
  if curl -fsS http://127.0.0.1:18763/health >"$work/health.txt" 2>/dev/null; then break; fi
  kill -0 "$server_pid"
  sleep 1
done
cat "$work/health.txt"
curl -fsS http://127.0.0.1:18763/health >/dev/null
printf '%s\n' "$server_pid" >"$work/server.pid"
printf 'server.pid=%s api=http://127.0.0.1:18763\n' "$server_pid"
if [[ "$action" == serve ]]; then
  wait "$server_pid"
  exit
fi
fixture=127.0.0.1:18762/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a
printf 'boot.begin=%s\n' "$(date -u +%FT%T.%NZ)"
sandbox=$(timeout 170 "$aenv" start --cold -d --timeout 300 "$fixture")
printf 'boot.end=%s sandbox=%s\n' "$(date -u +%FT%T.%NZ)" "$sandbox"
record_processes() {
  printf 'processes.%s=%s\n' "$1" "$(date -u +%FT%T.%NZ)"
  ps --ppid "$server_pid" -o pid=,comm=,args= || true
  for pid in $(ps --ppid "$server_pid" -o pid=,comm= | awk '$2 == "firecracker" {print $1}')
  do
    ls -l "/proc/$pid/fd" 2>/dev/null | grep -E 'vcpu|/dev/ublk' | sed "s/^/pid=$pid /" || true
    grep '/dev/ublk' "/proc/$pid/maps" 2>/dev/null | sed "s/^/pid=$pid map /" || true
  done
}
guest_summary='uname -r; cat /etc/os-release | head -3; cat /proc/cmdline; cat /sys/module/page_reporting/parameters/page_reporting_order; cat /sys/module/damon_reclaim/parameters/enabled'
record_processes booted
"$aenv" exec "$sandbox" /bin/sh -c "$guest_summary"
"$aenv" exec "$sandbox" /bin/sh -c 'printf "zb-stock-v025\n" >/tmp/zb-stock-marker; cat /proc/sys/kernel/random/boot_id' | tee "$work/boot-id.before"
printf 'pause.begin=%s\n' "$(date -u +%FT%T.%NZ)"
"$aenv" pause "$sandbox"
printf 'pause.end=%s\n' "$(date -u +%FT%T.%NZ)"
record_processes paused
find "$AENV_HOME_PATH" -xdev -type f \( -name 'vm_state*' -o -name 'mem*' -o -name '*.commit' -o -name '*.pack' \) -printf '%s\t%p\n' >"$work/checkpoint-files.txt"
"$aenv" list --output json
printf 'resume.begin=%s\n' "$(date -u +%FT%T.%NZ)"
"$aenv" resume "$sandbox" --timeout 420
printf 'resume.end=%s\n' "$(date -u +%FT%T.%NZ)"
record_processes resumed
"$aenv" exec "$sandbox" /bin/sh -c 'test "$(cat /tmp/zb-stock-marker)" = zb-stock-v025 && cat /proc/sys/kernel/random/boot_id' | tee "$work/boot-id.after"
cmp "$work/boot-id.before" "$work/boot-id.after"
"$aenv" exec "$sandbox" /bin/sh -c "$guest_summary"
printf 'snapshot.begin=%s\n' "$(date -u +%FT%T.%NZ)"
"$aenv" snapshot create "$sandbox" --name zb-stock-checkpoint | tee "$work/snapshot-created.txt"
printf 'snapshot.end=%s\n' "$(date -u +%FT%T.%NZ)"
"$aenv" snapshot list --output json
printf 'source.delete.begin=%s\n' "$(date -u +%FT%T.%NZ)"
"$aenv" delete "$sandbox"
printf 'source.delete.end=%s\n' "$(date -u +%FT%T.%NZ)"
record_processes source_deleted
snapshot_id=$(awk '/^Created snapshot / { print $3 }' "$work/snapshot-created.txt")
if [[ "$mode" == pack* ]]; then
  store="$AENV_HOME_PATH/snapshot-store/repository"
  manifest="$store/snapshots/$snapshot_id/memory-startup.pack"
  record="$store/catalog/records/$snapshot_id.json"
  printf 'pack.wait.begin=%s\n' "$(date -u +%FT%T.%NZ)"
  for _ in $(seq 150)
  do
    if [[ -s "$manifest" ]] && jq -e '.committed.memoryStartup != null or .committed.memory_startup != null' "$record" >/dev/null; then break; fi
    sleep 0.1
  done
  printf 'pack.wait.end=%s\n' "$(date -u +%FT%T.%NZ)"
  test -s "$manifest"
  stat -c 'startup_manifest_bytes=%s' "$manifest"
  sha256sum "$manifest"
  cat "$record"
  printf '\n'
fi
printf 'snapshot.restore.begin=%s\n' "$(date -u +%FT%T.%NZ)"
snapshot_vm=$(timeout 90 "$aenv" start -d zb-stock-checkpoint)
printf 'snapshot.restore.end=%s sandbox=%s\n' "$(date -u +%FT%T.%NZ)" "$snapshot_vm"
record_processes snapshot_restored
"$aenv" exec "$snapshot_vm" /bin/sh -c 'test "$(cat /tmp/zb-stock-marker)" = zb-stock-v025 && cat /proc/sys/kernel/random/boot_id' | tee "$work/boot-id.snapshot"
cmp "$work/boot-id.before" "$work/boot-id.snapshot"
if [[ "$mode" == pack* ]]; then
  for _ in $(seq 100)
  do
    if grep -q 'memory device startup prefetch drained' "$work/server.log"; then break; fi
    sleep 0.1
  done
  grep 'memory device startup prefetch drained' "$work/server.log"
fi
"$aenv" delete "$snapshot_vm"
"$aenv" list --output json
printf 'smoke.result=pass at %s\n' "$(date -u +%FT%T.%NZ)"
