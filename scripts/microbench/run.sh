#!/usr/bin/env bash
# Runs the microbenchmark tasks for one harness on one deployment backend,
# behind aries-replay, and lists every run in a manifest for analyze.py.
#
#   scripts/microbench/run.sh --harness openclaw --backend docker --mode record \
#     --address 10.0.1.3 --prometheus http://10.106.21.199:9090
#   scripts/microbench/run.sh --harness openclaw --backend k8s --mode replay \
#     --runs 10 --address 10.0.1.3 --prometheus http://10.106.21.199:9090
#
# Record once per harness, against the real model; then replay on every
# backend, so all of them see the same model output and latency. Run it from an
# ARIES checkout on the node the comparison measures, with bin/ built, the
# tasks packed (scripts/microbench/pack.sh) and, for record mode, the model key
# in DEEPSEEK_API_KEY or DEEPSEEK_API.key. Kubernetes runs use $KUBECONFIG.
# --bridge-repository maps the Kubernetes bridge image to another repository,
# such as one loaded onto the node, keeping the commit tag ARIES requests.
#
# Each harness is created while its sandbox starts, as ARIES does by default;
# --sequential creates it after the bridge registers the sandbox. --warm runs
# each task twice on the same harness and sandbox, the first run cold and the
# second warm; a plain recording serves it, since both runs are the same
# conversation. --tasks runs a comma-separated subset of the profile's tasks.
# Kubernetes pods are pinned to this node, where the Docker runs go.
set -euo pipefail

harness="" backend="" mode="" runs=1 address="" prometheus="" pace=recorded port=8090 bridge_repository=""
parallel=true warm=false tasks=""
usage() { sed -n '2,24s/^# \{0,1\}//p' "$0"; }
while [ $# -gt 0 ]; do
  case "$1" in
    --harness) harness=$2; shift 2 ;;
    --backend) backend=$2; shift 2 ;;
    --mode) mode=$2; shift 2 ;;
    --runs) runs=$2; shift 2 ;;
    --address) address=$2; shift 2 ;;
    --prometheus) prometheus=$2; shift 2 ;;
    --pace) pace=$2; shift 2 ;;
    --port) port=$2; shift 2 ;;
    --bridge-repository) bridge_repository=$2; shift 2 ;;
    --sequential) parallel=false; shift ;;
    --warm) warm=true; shift ;;
    --tasks) tasks=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done
case "$harness" in openclaw|hermes) ;; *) echo "--harness must be openclaw or hermes" >&2; exit 2 ;; esac
case "$backend" in docker|k8s) ;; *) echo "--backend must be docker or k8s" >&2; exit 2 ;; esac
case "$mode" in record|replay) ;; *) echo "--mode must be record or replay" >&2; exit 2 ;; esac
case "$runs" in ''|*[!0-9]*|0) echo "--runs must be a positive number" >&2; exit 2 ;; esac
[ -n "$address" ] || { echo "--address is required: this node's IP, which harness containers and pods reach aries-replay at" >&2; exit 2; }
[ -n "$prometheus" ] || { echo "--prometheus is required" >&2; exit 2; }
[ "$mode" = replay ] || [ "$runs" -eq 1 ] || { echo "record mode makes one run; replay the recording for repetitions" >&2; exit 2; }

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$root"
for binary in bin/aries bin/aries-replay; do
  [ -x "$binary" ] || { echo "$binary is missing; run make build" >&2; exit 1; }
done
[ -d .cache/microbench ] || { echo ".cache/microbench is missing; run scripts/microbench/pack.sh" >&2; exit 1; }

store_dir=.cache/microbench-replay/$harness
store=$store_dir/recordings.jsonl
mkdir -p "$store_dir" .cache/microbench-runs
if [ "$mode" = record ] && [ -s "$store" ]; then
  echo "$store already holds a recording; move it aside to record again" >&2
  exit 1
fi
if [ "$mode" = replay ] && [ ! -s "$store" ]; then
  echo "$store is missing; record first" >&2
  exit 1
fi

stamp=$(date -u +%Y%m%dT%H%M%SZ)
variant=$([ "$warm" = true ] && echo warm || echo cold)-$([ "$parallel" = true ] && echo parallel || echo sequential)
name=$harness-microbench-$backend-$mode-$variant
profile=.cache/microbench-runs/$name.json
python3 - "profiles/comparison/$harness-microbench-$backend.json" "$profile" "$name" "$address:$port" "$prometheus" "$root" "$bridge_repository" "$parallel" "$warm" "$(hostname -f)" "$tasks" <<'PY'
import json, sys
source, target, name, address, prometheus, root, bridge_repository, parallel, warm, node, tasks = sys.argv[1:]
profile = json.load(open(source))
profile["name"] = name
# The written profile lives elsewhere, so its relative paths become absolute.
profile["versions_file"] = root + "/configs/microbench-versions.json"
profile["overrides_file"] = root + "/profiles/comparison/" + profile["overrides_file"]
profile["model"]["base_url"] = "http://%s/v1" % address
placement = profile["sandbox"]["deployment"]
placement[placement["backend"]]["prometheus_url"] = prometheus
if bridge_repository:
    bridge = profile["bridge"]["deployment"]
    if bridge["backend"] != "kubernetes":
        sys.exit("--bridge-repository applies only to Kubernetes runs")
    bridge["kubernetes"]["images"] = {"aries-bridge": bridge_repository}
if tasks:
    subset = tasks.split(",")
    if any(task not in profile["benchmark"]["tasks"] for task in subset):
        sys.exit("--tasks must name tasks from " + ",".join(profile["benchmark"]["tasks"]))
    profile["benchmark"]["tasks"] = subset
execution = profile.setdefault("execution", {})
execution["parallel_start"] = parallel == "true"
if warm == "true":
    execution["warmup"] = {}
for role in ("harness", "sandbox", "bridge"):
    deployment = profile[role]["deployment"]
    if deployment["backend"] == "kubernetes":
        deployment["kubernetes"]["node_selector"] = {"kubernetes.io/hostname": node}
json.dump(profile, open(target, "w"), indent=2)
PY

if [ "$mode" = record ] && [ -z "${DEEPSEEK_API_KEY:-}" ]; then
  [ -f DEEPSEEK_API.key ] || { echo "record mode needs DEEPSEEK_API_KEY or DEEPSEEK_API.key" >&2; exit 1; }
  DEEPSEEK_API_KEY=$(tr -d '\n' < DEEPSEEK_API.key)
fi
# In replay mode the key is never sent anywhere; ARIES only requires one.
export DEEPSEEK_API_KEY=${DEEPSEEK_API_KEY:-unused}

requests=$store_dir/requests-$backend-$mode-$stamp.jsonl
# Another server on the port would answer the readiness check below in place
# of this one, and its recordings and request log are not this run's.
if curl -s -o /dev/null "http://127.0.0.1:$port/v1/models"; then
  echo "port $port already has a server; stop it first" >&2
  exit 1
fi
./bin/aries-replay -mode "$mode" -addr "0.0.0.0:$port" -store "$store" -log "$requests" -pace "$pace" -model deepseek-flash \
  >"$store_dir/server-$stamp.log" 2>&1 &
server=$!
trap 'kill "$server" 2>/dev/null || true; wait "$server" 2>/dev/null || true' EXIT
for _ in $(seq 50); do
  # Any HTTP answer will do: in record mode an unauthenticated probe is passed
  # to the upstream, which refuses it.
  curl -s -o /dev/null "http://127.0.0.1:$port/v1/models" && break
  kill -0 "$server" 2>/dev/null || { cat "$store_dir/server-$stamp.log" >&2; exit 1; }
  sleep 0.2
done

manifest=.cache/microbench-runs/manifest.tsv
[ -f "$manifest" ] || printf 'harness\tbackend\tmode\trepetition\trun_dir\trequests\tvariant\n' >"$manifest"
for repetition in $(seq "$runs"); do
  echo "==> $name, run $repetition of $runs"
  log=.cache/microbench-runs/$name-$stamp-$repetition.log
  status=0
  ./bin/aries "$profile" >"$log" 2>&1 || status=$?
  run_dir=$(sed -n 's/.*"output_dir":"\([^"]*\)".*/\1/p' "$log" | head -1)
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$harness" "$backend" "$mode" "$repetition" "$run_dir" "$requests" "$variant" >>"$manifest"
  echo "    ${run_dir:-no run directory} (exit $status)"
done
