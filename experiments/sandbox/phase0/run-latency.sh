#!/usr/bin/env bash
# Runs the Phase 0 latency benchmark on every backend; one JSON line per
# round and a summary per backend and concurrency, into latency.jsonl.
# HOST is the worker the Kubernetes, Docker and OpenSandbox probes run on, and
# CUBE_HOST the standalone CubeSandbox node; both are ssh targets.
HOST=${HOST:?set HOST to the worker}
CUBE_HOST=${CUBE_HOST:?set CUBE_HOST to the CubeSandbox node}
S=$(dirname "$0"); out=$S/latency.jsonl; : > $out
NODE=${NODE:?set NODE to the worker Kubernetes node name}
CUBE='cd ~/phase0; E2B_API_URL=http://127.0.0.1:3000 E2B_API_KEY=e2b_000000 CUBE_TEMPLATE_ID=probe-py SSL_CERT_FILE=$HOME/cube/rootCA.pem ~/cube/venv/bin/python probe.py cube latency'
( for c in 1 10; do ssh $CUBE_HOST "$CUBE --iterations 50 --concurrency $c" 2>/dev/null; done >> $S/latency-cube.jsonl ) &
for c in 1 10; do
  for b in docker opensandbox; do
    ssh $HOST "cd ~/phase0; python3 probe.py $b latency --iterations 50 --concurrency $c" 2>/dev/null >> $out
  done
  ssh $HOST "cd ~/phase0; PROBE_NODE=$NODE python3 probe.py agentsandbox-claim pool --replicas 12; kubectl -n aries-probe wait sandbox --all --for=condition=Ready --timeout=600s >/dev/null 2>&1; sleep 20" 2>/dev/null
  for b in pod pod-gvisor pod-kata agentsandbox agentsandbox-gvisor agentsandbox-kata agentsandbox-claim; do
    ssh $HOST "cd ~/phase0; PROBE_NODE=$NODE python3 probe.py $b latency --iterations 50 --concurrency $c" 2>/dev/null >> $out
  done
done
wait
cat $S/latency-cube.jsonl >> $out
echo done
