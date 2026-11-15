#!/usr/bin/env python3
"""Runs an experiment campaign on a cluster and copies its results back.

    python3 scripts/experiment/run.py start experiments/coldwarm [--campaign NAME]
        [--cluster k8s/cluster.json] [--recordings-from experiments/coldwarm/results/OLD]
        [--prometheus URL] [--allow-dirty] [--dry-run]
    python3 scripts/experiment/run.py status  experiments/coldwarm/results/NAME
    python3 scripts/experiment/run.py collect experiments/coldwarm/results/NAME [--watch SECONDS]
    python3 scripts/experiment/run.py resume  experiments/coldwarm/results/NAME [--prepare]
    python3 scripts/experiment/run.py stop    experiments/coldwarm/results/NAME
    python3 scripts/experiment/run.py mark    experiments/coldwarm/results/NAME RUN --status STATUS --reason TEXT

An experiment is a directory under experiments/ whose experiment.json names
the harnesses, setups (docker, k8s, native), tasks, repetitions and whether
each task also runs warm. A campaign is one execution of it, in
experiments/<experiment>/results/<campaign>/, beside the spec (docs/experiments.md
describes the layout).

start runs on this machine. It ships the committed tree to the cluster's
workers (from k8s/cluster.json) with the tasks packed here, builds ARIES there, and
starts one chain per worker in the background: each harness runs on a worker
of its own, and all setups of a harness on the same worker, so setups are
compared on the same hardware. A chain records the harness's model replies
once (or installs the recordings of --recordings-from), then runs every
repetition of every setup in turn, the setups interleaved so drift affects
them alike. Every run is listed with its status in the campaign's manifest.
A chain skips the steps it has finished, so resume restarts it after a failure
or a reboot and retries only the failed steps.

collect copies every worker's campaign directory back and merges the manifest;
--watch repeats it until every chain is done. mark sets a run's status, kept
in marks.tsv so it survives later collects: valid runs are analysed, failed
and superseded ones are kept but left out. A run is named by its path, such
as runs/openclaw/docker/3. scripts/experiment/report.py then analyses the
valid runs and writes the report. Standard library only; needs ssh and rsync.
"""

import argparse
import datetime
import glob
import hashlib
import json
import os
import shlex
import subprocess
import sys
import tempfile
import threading
import time

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
HARNESSES = ("openclaw", "hermes")
SETUPS = ("docker", "k8s", "native")
STATUSES = ("valid", "failed", "superseded")
# Harnesses always start alongside their sandboxes, and recordings are always
# made on Docker, so neither is a key.
SPEC_KEYS = ("name", "description", "harnesses", "setups", "tasks", "repetitions", "warm", "record_warm",
             "bridge_repository")
COLUMNS = ["harness", "setup", "repetition", "mode", "variant", "status", "reason", "run_dir", "requests",
           "started", "finished", "worker"]
REMOTE_ARIES = "aries"
REMOTE_CAMPAIGNS = "aries-campaigns"
PROMETHEUS_SERVICE = ("monitoring", "prometheus-kube-prometheus-prometheus", 9090)


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def fail(message):
    sys.exit("run.py: " + message)


# ---------------------------------------------------------------- the spec

def load_spec(path):
    if os.path.isdir(path):
        path = os.path.join(path, "experiment.json")
    with open(path) as handle:
        spec = json.load(handle)
    for key in ("name", "harnesses", "setups", "tasks", "repetitions"):
        if key not in spec:
            fail("%s has no %s" % (path, key))
    if not spec["harnesses"] or any(h not in HARNESSES for h in spec["harnesses"]):
        fail("harnesses must be a non-empty subset of " + ", ".join(HARNESSES))
    if not spec["setups"] or any(s not in SETUPS for s in spec["setups"]):
        fail("setups must be a non-empty subset of " + ", ".join(SETUPS))
    if "native" in spec["setups"]:
        # The native baselines take their harness configuration from an ARIES
        # Docker replay, so one must have run first.
        if "docker" not in spec["setups"] or spec["setups"].index("docker") > spec["setups"].index("native"):
            fail("native needs docker listed before it: it reuses the configuration of an ARIES Docker run")
    if not spec["tasks"]:
        fail("tasks must not be empty")
    if not isinstance(spec["repetitions"], int) or spec["repetitions"] < 1:
        fail("repetitions must be a positive number")
    unknown = set(spec) - set(SPEC_KEYS)
    if unknown:
        fail("%s has unknown keys %s; it takes %s" % (path, ", ".join(sorted(unknown)), ", ".join(SPEC_KEYS)))
    spec.setdefault("description", "")
    spec.setdefault("warm", False)
    spec.setdefault("record_warm", spec["warm"])
    spec.setdefault("bridge_repository", "")
    for key in ("warm", "record_warm"):
        if not isinstance(spec[key], bool):
            fail("%s must be true or false" % key)
    spec["_path"] = os.path.relpath(path, ROOT)
    spec["_dir"] = os.path.dirname(os.path.abspath(path))
    return spec


# ------------------------------------------------------------ the cluster

def load_cluster(path):
    with open(path) as handle:
        cluster = json.load(handle)
    if not cluster.get("master") or not cluster.get("workers"):
        fail("%s needs master and workers" % path)
    return cluster


def ssh_command(state):
    command = [os.environ.get("EXPERIMENT_SSH", "ssh"), "-o", "BatchMode=yes"]
    if state.get("ssh_key"):
        command += ["-i", os.path.expanduser(state["ssh_key"])]
    return command + list(state.get("ssh_options") or [])


def remote(state, target, command, data=None, check=True):
    """Runs a shell command on a node and returns its standard output."""
    result = subprocess.run(ssh_command(state) + [target, command], input=data, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, stdin=None if data is not None else subprocess.DEVNULL)
    if check and result.returncode != 0:
        output = (result.stdout + result.stderr).decode(errors="replace").strip().splitlines()
        raise RuntimeError("%s: %s failed (exit %d):\n    %s" % (
            target, command.strip().split("\n")[0][:80], result.returncode, "\n    ".join(output[-15:])))
    return result.stdout.decode(errors="replace")


def short(target):
    return target.split("@")[-1].split(".")[0]


# -------------------------------------------------------------- the chain

CHAIN = r'''#!/usr/bin/env bash
# Generated by scripts/experiment/run.py: campaign @CAMPAIGN@ of @EXPERIMENT@
# on @WORKER@. Running it again skips the steps that finished.
# /etc/profile puts Go on the path; it is read before set -u, which it may
# not survive.
source /etc/profile >/dev/null 2>&1 || true
set -uo pipefail
aries=@ARIES@
cdir=@CDIR@
logs=$cdir/logs/@WORKER@
manifest=$logs/manifest.tsv
node_manifest=$aries/.cache/microbench-runs/manifest.tsv
export KUBECONFIG=$aries/kubeconfig
cd "$aries" || exit 1
mkdir -p "$logs/steps" "$cdir/runs" "$cdir/recordings" "$(dirname "$node_manifest")"
ps -o pgid= $$ | tr -d ' ' >"$logs/chain.pgid"
echo @TOTAL@ >"$logs/steps.total"
[ -f "$manifest" ] || printf '@HEADER@\n' >"$manifest"
echo "######## start $(date -u +%FT%TZ)"

# Lists a run in the campaign manifest and copies it, with its model request
# log, to runs/<harness>/<setup>/<repetition> (a retry gets a suffix).
list_run() {
  local harness=$1 setup=$2 repetition=$3 mode=$4 variant=$5 run_dir=$6 requests=$7 problem=$8 started=$9 finished=${10}
  local status=valid dest="" kept=""
  if [ -z "$run_dir" ] || [ ! -f "$run_dir/run-result.json" ]; then
    status=failed problem=${problem:-no run result}
  elif [ -n "$problem" ]; then
    status=failed
  fi
  if [ -n "$run_dir" ] && [ -d "$run_dir" ]; then
    dest=runs/$harness/$setup/$repetition
    local n=2
    while [ -e "$cdir/$dest" ]; do dest=runs/$harness/$setup/$repetition-$n; n=$((n + 1)); done
    mkdir -p "$cdir/$(dirname "$dest")"
    cp -a "$run_dir" "$cdir/$dest"
    if [ -n "$requests" ] && [ -f "$requests" ]; then cp "$requests" "$cdir/$dest/requests.jsonl"; kept=$dest/requests.jsonl; fi
  fi
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$harness" "$setup" "$repetition" "$mode" "$variant" \
    "$status" "$problem" "$dest" "$kept" "$started" "$finished" @WORKER@ >>"$manifest"
}

# step ID HARNESS SETUP REPETITION COMMAND...: runs a command that lists its
# runs in the node's microbenchmark manifest, and lists them in the campaign's.
step() {
  local id=$1 harness=$2 setup=$3 repetition=$4
  shift 4
  [ -e "$logs/steps/$id.done" ] && return 0
  [ -f "$node_manifest" ] || printf 'harness\tbackend\tmode\trepetition\trun_dir\trequests\tvariant\n' >"$node_manifest"
  local before started finished status=0 listed=0 problem=""
  before=$(wc -l <"$node_manifest")
  started=$(date -u +%FT%TZ)
  echo "==> $id ($started)"
  echo "######## attempt $started" >>"$logs/steps/$id.log"
  "$@" >>"$logs/steps/$id.log" 2>&1 || status=$?
  finished=$(date -u +%FT%TZ)
  [ "$status" -eq 0 ] || problem="exit $status"
  # run.sh itself succeeds when a run fails; it prints ARIES's exit status.
  local aries_status
  aries_status=$(sed -n 's/.*(exit \([0-9]*\))$/\1/p' "$logs/steps/$id.log" | tail -1)
  [ -z "$aries_status" ] || [ "$aries_status" = 0 ] || problem="aries exit $aries_status"
  # A tab is whitespace to read, which would merge an empty field into the
  # next, so the fields are split at a unit separator instead.
  while IFS=$'\037' read -r _ _ mode _ run_dir requests variant; do
    listed=1
    list_run "$harness" "$setup" "$repetition" "$mode" "$variant" "$run_dir" "$requests" "$problem" "$started" "$finished"
  done < <(tail -n +$((before + 1)) "$node_manifest" | tr '\t' '\037')
  if [ "$listed" -eq 0 ]; then
    [ "$status" -ne 0 ] || status=1
    list_run "$harness" "$setup" "$repetition" "" "" "" "" "${problem:-no run listed}" "$started" "$finished"
  fi
  if [ "$status" -eq 0 ] && [ -z "$problem" ]; then
    rm -f "$logs/steps/$id.failed"; touch "$logs/steps/$id.done"; echo "    done"
  else
    touch "$logs/steps/$id.failed"; echo "    failed: ${problem:-exit $status} (see logs/@WORKER@/steps/$id.log)"
    return 1
  fi
}

# Installs the campaign's recording as the harness's replay store, moving any
# other recording aside, or reports that the harness must record.
install_recording() {
  local harness=$1 store=$aries/.cache/microbench-replay/$1/recordings.jsonl
  mkdir -p "$(dirname "$store")"
  if [ -s "$cdir/recordings/$harness.jsonl" ]; then
    if [ -s "$store" ] && ! cmp -s "$store" "$cdir/recordings/$harness.jsonl"; then
      mv "$store" "$store.before-@CAMPAIGN@-$(date -u +%Y%m%dT%H%M%SZ)"
    fi
    cp "$cdir/recordings/$harness.jsonl" "$store"
    touch "$logs/steps/$harness-record.done"
    return 0
  fi
  [ ! -s "$store" ] || mv "$store" "$store.before-@CAMPAIGN@-$(date -u +%Y%m%dT%H%M%SZ)"
  return 1
}

@BODY@
echo "######## done $(date -u +%FT%TZ)"
'''


def chain_body(spec, harnesses, address, prometheus, port):
    """The steps of one worker's chain, and how many there are."""
    common = "--address %s --prometheus %s" % (shlex.quote(address), shlex.quote(prometheus))
    tasks = "--tasks " + shlex.quote(",".join(spec["tasks"]))
    warm = " --warm" if spec["warm"] else ""
    lines, total = [], 0
    for harness in harnesses:
        lines += ["", "# ---- %s" % harness,
                  "if ! install_recording %s; then" % harness,
                  "  step %s-record %s docker record scripts/microbench/run.sh --harness %s --backend docker --mode record%s %s %s --port %d || {"
                  % (harness, harness, harness, " --warm" if spec["record_warm"] else "", tasks, common, port),
                  "    echo \"recording %s failed; skipping its runs\"" % harness,
                  "    harness_failed=1",
                  "  }",
                  "  [ -n \"${harness_failed:-}\" ] || cp .cache/microbench-replay/%s/recordings.jsonl \"$cdir/recordings/%s.jsonl\"" % (harness, harness),
                  "fi",
                  "if [ -z \"${harness_failed:-}\" ]; then"]
        total += 1
        for repetition in range(1, spec["repetitions"] + 1):
            for setup in spec["setups"]:
                identifier = "%s-%s-%d" % (harness, setup, repetition)
                if setup == "native":
                    command = "python3 scripts/baseline/%s_native.py --runs 1%s %s %s --port %d" % (harness, warm, tasks, common, port)
                else:
                    bridge = ""
                    if setup == "k8s" and spec["bridge_repository"]:
                        bridge = " --bridge-repository " + shlex.quote(spec["bridge_repository"])
                    command = "scripts/microbench/run.sh --harness %s --backend %s --mode replay --runs 1%s%s %s %s --port %d" % (
                        harness, setup, warm, bridge, tasks, common, port)
                lines.append("  step %s %s %s %d %s" % (identifier, harness, setup, repetition, command))
                total += 1
        lines += ["fi", "unset harness_failed"]
    return "\n".join(lines), total


def render_chain(spec, campaign, worker, harnesses, address, prometheus, port=8090):
    body, total = chain_body(spec, harnesses, address, prometheus, port)
    values = {
        "@CAMPAIGN@": campaign, "@EXPERIMENT@": spec["name"], "@WORKER@": worker, "@TOTAL@": str(total),
        "@ARIES@": "$HOME/" + REMOTE_ARIES, "@CDIR@": "$HOME/%s/%s/%s" % (REMOTE_CAMPAIGNS, spec["name"], campaign),
        "@HEADER@": "\\t".join(COLUMNS[:-1]) + "\\tworker", "@BODY@": body,
    }
    text = CHAIN
    for key, value in values.items():
        text = text.replace(key, value)
    return text


def remote_campaign(spec_name, campaign):
    return "%s/%s/%s" % (REMOTE_CAMPAIGNS, spec_name, campaign)


# ------------------------------------------------------- campaign records

def campaign_paths(directory):
    return {name: os.path.join(directory, name) for name in
            ("meta.json", "state.json", "manifest.tsv", "marks.tsv", "experiment.json")}


def read_json(path):
    with open(path) as handle:
        return json.load(handle)


def write_json(path, value):
    with open(path, "w") as handle:
        json.dump(value, handle, indent=2)
        handle.write("\n")


def read_tsv(path):
    if not os.path.exists(path):
        return []
    with open(path) as handle:
        lines = handle.read().splitlines()
    if not lines:
        return []
    header = lines[0].split("\t")
    return [dict(zip(header, line.split("\t") + [""] * (len(header) - len(line.split("\t"))))) for line in lines[1:] if line]


def write_tsv(path, rows, columns):
    with open(path, "w") as handle:
        handle.write("\t".join(columns) + "\n")
        for row in rows:
            handle.write("\t".join(str(row.get(column, "")) for column in columns) + "\n")


def merge_manifest(directory):
    """Joins the workers' manifests, then applies retries and marks: a failed
    attempt is superseded by a later valid one of the same run, and marks.tsv
    has the last word."""
    paths = campaign_paths(directory)
    rows = []
    for path in sorted(glob.glob(os.path.join(directory, "logs", "*", "manifest.tsv"))):
        rows.extend(read_tsv(path))
    rows.sort(key=lambda row: row.get("started", ""))
    for row in rows:
        if row["status"] == "valid":
            for earlier in rows:
                if earlier is row:
                    break
                if (earlier["harness"], earlier["setup"], earlier["repetition"]) == (row["harness"], row["setup"], row["repetition"]) \
                        and earlier["status"] == "failed":
                    earlier["status"], earlier["reason"] = "superseded", "retried: " + (earlier["reason"] or "failed")
    marks = {mark["run_dir"]: mark for mark in read_tsv(paths["marks.tsv"])}
    for row in rows:
        mark = marks.get(row["run_dir"])
        if mark:
            row["status"], row["reason"] = mark["status"], mark["reason"]
    write_tsv(paths["manifest.tsv"], rows, COLUMNS)
    return rows


def checksum(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


# ------------------------------------------------------------- commands

def git(*args):
    return subprocess.run(["git", "-C", ROOT] + list(args), check=True, stdout=subprocess.PIPE).stdout.decode().strip()


def node_facts(state, target):
    # Each fact is one line, "unknown" when its command prints nothing.
    script = r'''
one() { v=$(eval "$1" 2>/dev/null | head -1); echo "${v:-unknown}"; }
one hostname
one "lscpu | sed -n 's/^Model name: *//p'"
one nproc
one "awk '/MemTotal/ {printf \"%.1f\\n\", \$2 / 1048576}' /proc/meminfo"
one "uname -r"
one "kubelet --version | sed 's/^Kubernetes //'"
one "sg docker -c 'docker version --format {{.Server.Version}}'"
one "cat /sys/devices/system/cpu/smt/control"
if [ -r /sys/devices/system/cpu/intel_pstate/no_turbo ]; then
  [ "$(cat /sys/devices/system/cpu/intel_pstate/no_turbo)" = 1 ] && echo off || echo on
elif [ -r /sys/devices/system/cpu/cpufreq/boost ]; then
  [ "$(cat /sys/devices/system/cpu/cpufreq/boost)" = 0 ] && echo off || echo on
else echo unknown; fi
'''
    lines = remote(state, target, script).splitlines() + ["unknown"] * 9
    keys = ["hostname", "cpu", "cpus", "memory_gib", "kernel", "kubelet", "docker", "smt", "turbo"]
    return dict(zip(keys, (line.strip() for line in lines)))


def packed_tasks():
    """The microbenchmark tasks packed here, as a tar of the checkout the
    profiles run from: packing reads Git history, which the shipped tree
    lacks."""
    with tempfile.TemporaryDirectory() as work:
        destination = os.path.join(work, "microbench")
        subprocess.run([os.path.join(ROOT, "scripts", "microbench", "pack.sh"), destination], check=True,
                       stdout=subprocess.DEVNULL)
        return subprocess.run(["tar", "-C", work, "-cf", "-", "microbench"], check=True, stdout=subprocess.PIPE,
                              env=dict(os.environ, COPYFILE_DISABLE="1")).stdout


def prepare_worker(state, label, target, revision, tasks, log):
    """Ships the committed tree and the packed tasks, and builds ARIES."""
    archive = subprocess.run(["git", "-C", ROOT, "archive", "HEAD"], check=True, stdout=subprocess.PIPE).stdout
    log.write(remote(state, target, "mkdir -p ~/%s && cd ~/%s && tar -xf -" % (REMOTE_ARIES, REMOTE_ARIES), data=archive))
    log.write(remote(state, target, "cd ~/%s && mkdir -p .cache && rm -rf .cache/microbench && tar -xf - -C .cache"
                     % REMOTE_ARIES, data=tasks))
    # The checkout has no Git history either, so the bridge revision, which
    # tags the Kubernetes bridge image the runner requests, is passed in.
    log.write(remote(state, target, "source /etc/profile; cd ~/%s && make build REVISION=%s 2>&1"
                     % (REMOTE_ARIES, shlex.quote(revision))))


def cmd_start(args):
    spec = load_spec(args.experiment)
    cluster = load_cluster(args.cluster)
    commit = git("rev-parse", "HEAD")
    dirty = bool(git("status", "--porcelain", "--untracked-files=no"))
    if dirty and not args.allow_dirty and not args.dry_run:
        fail("the working tree has uncommitted changes, which the nodes would not get; commit them or pass --allow-dirty")
    campaign = args.campaign or datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d-%H%M")
    directory = os.path.join(spec["_dir"], "results", campaign)
    if os.path.exists(directory) and not args.dry_run:
        fail("%s exists; use resume, or another --campaign" % os.path.relpath(directory, ROOT))
    recordings = {}
    if args.recordings_from:
        for harness in spec["harnesses"]:
            path = os.path.join(args.recordings_from, "recordings", harness + ".jsonl")
            if not os.path.exists(path):
                fail("%s has no recording for %s" % (args.recordings_from, harness))
            recordings[harness] = path
    workers = cluster["workers"]
    assignment = {harness: workers[index % len(workers)] for index, harness in enumerate(spec["harnesses"])}
    state = {"master": cluster["master"], "ssh_key": cluster.get("ssh_key", ""),
             "ssh_options": cluster.get("ssh_options") or [], "workers": {}}
    for harness, target in assignment.items():
        state["workers"].setdefault(short(target), {"target": target, "harnesses": []})["harnesses"].append(harness)

    if args.dry_run:
        for label, worker in state["workers"].items():
            print("#### %s (%s): %s" % (label, worker["target"], ", ".join(worker["harnesses"])))
            print(render_chain(spec, campaign, label, worker["harnesses"], "<node address>",
                               args.prometheus or "<prometheus url>"))
        return

    revision = subprocess.run([os.path.join(ROOT, "scripts", "bridge-revision.sh")], check=True,
                              stdout=subprocess.PIPE, cwd=ROOT).stdout.decode().strip()
    prometheus = args.prometheus
    if not prometheus:
        namespace, service, port = PROMETHEUS_SERVICE
        address = remote(state, state["master"], "kubectl -n %s get svc %s -o jsonpath='{.spec.clusterIP}'"
                         % (namespace, service)).strip()
        if not address:
            fail("no Prometheus service on the cluster; pass --prometheus")
        prometheus = "http://%s:%d" % (address, port)

    tasks = packed_tasks()
    os.makedirs(os.path.join(directory, "logs"))
    os.makedirs(os.path.join(directory, "recordings"))
    for harness, path in recordings.items():
        with open(path, "rb") as source, open(os.path.join(directory, "recordings", harness + ".jsonl"), "wb") as copy:
            copy.write(source.read())
    with open(os.path.join(directory, "experiment.json"), "w") as handle:
        json.dump({k: v for k, v in spec.items() if not k.startswith("_")}, handle, indent=2)
        handle.write("\n")
    write_json(campaign_paths(directory)["state.json"], state)

    versions = read_json(os.path.join(ROOT, "configs", "microbench-versions.json"))
    meta = {
        "experiment": spec["name"], "campaign": campaign, "spec": spec["_path"],
        "aries_commit": commit, "aries_dirty": dirty, "bridge_revision": revision,
        "bridge_image": "%s-%s" % (versions.get("bridge", {}).get("image", "aries-bridge"), revision),
        "vhive_revision": vhive_revision(), "kubernetes_base_image": versions.get("bridge", {}).get("kubernetes_base_image"),
        "prometheus": prometheus, "started_at": now(), "finished_at": None,
        "recordings": {"source": os.path.relpath(args.recordings_from, ROOT) if args.recordings_from else "recorded in this campaign"},
        "workers": {},
    }

    errors = []

    def prepare(label, worker):
        target = worker["target"]
        try:
            with open(os.path.join(directory, "logs", label + "-prepare.log"), "w") as log:
                print("==> %s: shipping %s and building" % (label, commit[:12]), flush=True)
                prepare_worker(state, label, target, revision, tasks, log)
            facts = node_facts(state, target)
            node = facts["hostname"]
            address = remote(state, state["master"], "kubectl get node %s -o jsonpath='{.status.addresses[?(@.type==\"InternalIP\")].address}'"
                             % shlex.quote(node)).strip()
            if not address:
                raise RuntimeError("%s: no Kubernetes node named %s" % (label, node))
            if "k8s" in spec["setups"]:
                kubeconfig = remote(state, state["master"], "cat ~/aries-kubeconfig").encode()
                remote(state, target, "umask 077; cat > ~/%s/kubeconfig" % REMOTE_ARIES, data=kubeconfig)
            needs_key = any(harness not in recordings for harness in worker["harnesses"])
            if needs_key:
                key = os.path.join(ROOT, "DEEPSEEK_API.key")
                if not os.path.exists(key):
                    raise RuntimeError("recording needs DEEPSEEK_API.key, or pass --recordings-from")
                with open(key, "rb") as handle:
                    remote(state, target, "umask 077; cat > ~/%s/DEEPSEEK_API.key" % REMOTE_ARIES, data=handle.read())
            base = remote_campaign(spec["name"], campaign)
            remote(state, target, "mkdir -p ~/%s/recordings ~/%s/logs/%s" % (base, base, label))
            for harness in worker["harnesses"]:
                if harness in recordings:
                    with open(recordings[harness], "rb") as handle:
                        remote(state, target, "cat > ~/%s/recordings/%s.jsonl" % (base, harness), data=handle.read())
            chain = render_chain(spec, campaign, label, worker["harnesses"], address, prometheus)
            with open(os.path.join(directory, "logs", label + "-chain.sh"), "w") as handle:
                handle.write(chain)
            remote(state, target, "cat > ~/%s/logs/%s/chain.sh && chmod +x ~/%s/logs/%s/chain.sh" % (base, label, base, label),
                   data=chain.encode())
            worker.update({"node": node, "address": address})
            meta["workers"][label] = dict(facts, address=address, harnesses=worker["harnesses"], aries_commit=commit)
        except Exception as error:  # reported together below
            errors.append(str(error))

    threads = [threading.Thread(target=prepare, args=item) for item in state["workers"].items()]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join()
    write_json(campaign_paths(directory)["state.json"], state)
    write_json(campaign_paths(directory)["meta.json"], meta)
    if errors:
        fail("preparing the workers failed; fix it and run resume --prepare:\n  " + "\n  ".join(errors))
    for label, worker in state["workers"].items():
        launch(state, spec["name"], campaign, label, worker)
    rel = os.path.relpath(directory, ROOT)
    print("campaign %s started. Next:\n  python3 scripts/experiment/run.py status %s\n"
          "  python3 scripts/experiment/run.py collect %s --watch 300\n"
          "  python3 scripts/experiment/report.py %s" % (campaign, rel, rel, rel))


def vhive_revision():
    with open(os.path.join(ROOT, "k8s", "create-cluster.sh")) as handle:
        for line in handle:
            if line.startswith("VHIVE_REVISION="):
                return line.split(":-", 1)[-1].rstrip("}\n")
    return None


def chain_paths(spec_name, campaign, label):
    base = "~/%s/logs/%s" % (remote_campaign(spec_name, campaign), label)
    return base + "/chain.sh", base + "/chain.log", base + "/chain.pgid"


def group(pgid_path):
    """Shell that sets g to the chain's process group, failing when there is
    none or it is this shell's own, which signalling would end too."""
    return ("g=$(cat %s 2>/dev/null) && [ -n \"$g\" ] && [ \"$g\" != \"$(ps -o pgid= $$ | tr -d ' ')\" ]" % pgid_path)


def running(state, target, pgid_path):
    # A live process group means a live chain. Matching the command line
    # instead would match this ssh session too.
    out = remote(state, target, group(pgid_path) + " && pgrep -g \"$g\" | head -1", check=False)
    return bool(out.strip())


def launch(state, spec_name, campaign, label, worker):
    script, log, pgid = chain_paths(spec_name, campaign, label)
    if running(state, worker["target"], pgid):
        print("==> %s: chain already running" % label)
        return
    # setsid puts the chain in a process group of its own, which stop signals.
    remote(state, worker["target"], "(setsid nohup sg docker -c 'bash %s' >>%s 2>&1 </dev/null &)" % (script, log))
    print("==> %s: chain started (%s)" % (label, ", ".join(worker["harnesses"])))


def open_campaign(directory):
    paths = campaign_paths(directory)
    for name in ("meta.json", "state.json", "experiment.json"):
        if not os.path.exists(paths[name]):
            fail("%s is not a campaign started on this machine (no %s)" % (directory, name))
    return read_json(paths["meta.json"]), read_json(paths["state.json"])


def cmd_status(args):
    meta, state = open_campaign(args.campaign)
    for label, worker in state["workers"].items():
        script, log, pgid = chain_paths(meta["experiment"], meta["campaign"], label)
        base = os.path.dirname(script)
        out = remote(state, worker["target"], "cat %s/steps.total 2>/dev/null || echo 0; ls %s/steps/ 2>/dev/null | grep -c '[.]done$'; "
                     "ls %s/steps/ 2>/dev/null | grep -c '[.]failed$'; tail -1 %s 2>/dev/null" % (base, base, base, log), check=False)
        lines = out.splitlines() + ["", "", "", ""]
        alive = running(state, worker["target"], pgid)
        print("%-10s %-18s %s/%s steps done, %s failed, %s; last: %s" % (
            label, ",".join(worker["harnesses"]), lines[1], lines[0], lines[2], "running" if alive else "stopped", lines[3]))


def collect_once(directory, meta, state):
    for label, worker in state["workers"].items():
        base = remote_campaign(meta["experiment"], meta["campaign"])
        shell = " ".join(shlex.quote(part) for part in ssh_command(state))
        subprocess.run(["rsync", "-a", "-e", shell, "%s:%s/" % (worker["target"], base), directory + "/"], check=True)
    rows = merge_manifest(directory)
    meta["recordings"]["sha256"] = {os.path.basename(path)[:-len(".jsonl")]: checksum(path)
                                    for path in sorted(glob.glob(os.path.join(directory, "recordings", "*.jsonl")))}
    done = all(os.path.exists(os.path.join(directory, "logs", label, "chain.log"))
               and "######## done" in open(os.path.join(directory, "logs", label, "chain.log")).read()
               for label in state["workers"])
    if done and not meta.get("finished_at"):
        meta["finished_at"] = max((row["finished"] for row in rows if row.get("finished")), default=now())
    write_json(campaign_paths(directory)["meta.json"], meta)
    counts = {}
    for row in rows:
        counts[row["status"]] = counts.get(row["status"], 0) + 1
    print("%s: %d runs (%s)%s" % (now(), len(rows), ", ".join("%d %s" % (n, s) for s, n in sorted(counts.items())),
                                  ", all chains done" if done else ""), flush=True)
    return done


def cmd_collect(args):
    while True:
        # Read afresh each round: resume may have changed meta.json meanwhile.
        meta, state = open_campaign(args.campaign)
        done = collect_once(args.campaign, meta, state)
        if not args.watch or done:
            return
        time.sleep(args.watch)


def cmd_resume(args):
    meta, state = open_campaign(args.campaign)
    workers = {label: worker for label, worker in state["workers"].items() if not args.worker or label in args.worker}
    if args.worker and set(args.worker) - set(workers):
        fail("no worker %s; the campaign's workers are %s" % (", ".join(sorted(set(args.worker) - set(workers))),
                                                             ", ".join(state["workers"])))
    if args.prepare:
        if git("status", "--porcelain", "--untracked-files=no"):
            fail("the working tree has uncommitted changes, which the nodes would not get; commit them first")
        commit = git("rev-parse", "HEAD")
        revision = subprocess.run([os.path.join(ROOT, "scripts", "bridge-revision.sh")], check=True,
                                  stdout=subprocess.PIPE, cwd=ROOT).stdout.decode().strip()
        if revision != meta["bridge_revision"]:
            fail("this checkout's bridge revision is %s, but the campaign runs %s" % (revision, meta["bridge_revision"]))
        tasks = packed_tasks()
        for label, worker in workers.items():
            print("==> %s: shipping %s and building" % (label, commit[:12]), flush=True)
            with open(os.path.join(args.campaign, "logs", label + "-prepare.log"), "a") as log:
                prepare_worker(state, label, worker["target"], revision, tasks, log)
            # A worker prepared again runs a later commit than the campaign
            # started at; the report names it.
            meta["workers"].setdefault(label, {})["aries_commit"] = commit
        write_json(campaign_paths(args.campaign)["meta.json"], meta)
    for label, worker in workers.items():
        base = remote_campaign(meta["experiment"], meta["campaign"])
        chain = os.path.join(args.campaign, "logs", label + "-chain.sh")
        if os.path.exists(chain):
            with open(chain, "rb") as handle:
                remote(state, worker["target"], "mkdir -p ~/%s/logs/%s && cat > ~/%s/logs/%s/chain.sh" % (base, label, base, label),
                       data=handle.read())
        launch(state, meta["experiment"], meta["campaign"], label, worker)


def cmd_stop(args):
    meta, state = open_campaign(args.campaign)
    for label, worker in state["workers"].items():
        _, _, pgid = chain_paths(meta["experiment"], meta["campaign"], label)
        if not running(state, worker["target"], pgid):
            print("==> %s: not running" % label)
            continue
        # The chain started in the background, where interrupts are ignored,
        # so it is terminated: ARIES cleans its sandboxes up on SIGTERM as on
        # an interrupt. Whatever is left after 90 seconds is killed.
        remote(state, worker["target"], group(pgid) + " && kill -TERM -- -\"$g\"", check=False)
        for _ in range(45):
            if not running(state, worker["target"], pgid):
                break
            time.sleep(2)
        else:
            remote(state, worker["target"], group(pgid) + " && kill -KILL -- -\"$g\"", check=False)
        print("==> %s: stopped; resume reruns the interrupted step" % label)


def cmd_mark(args):
    paths = campaign_paths(args.campaign)
    run = os.path.relpath(os.path.abspath(args.run), args.campaign) if os.path.isabs(args.run) else args.run.rstrip("/")
    rows = read_tsv(paths["manifest.tsv"])
    if not any(row["run_dir"] == run for row in rows):
        fail("no run %s in %s" % (run, paths["manifest.tsv"]))
    marks = [mark for mark in read_tsv(paths["marks.tsv"]) if mark["run_dir"] != run]
    marks.append({"run_dir": run, "status": args.status, "reason": args.reason.replace("\t", " "), "marked": now()})
    write_tsv(paths["marks.tsv"], marks, ["run_dir", "status", "reason", "marked"])
    if glob.glob(os.path.join(args.campaign, "logs", "*", "manifest.tsv")):
        merge_manifest(args.campaign)
    else:
        for row in rows:
            if row["run_dir"] == run:
                row["status"], row["reason"] = args.status, args.reason
        write_tsv(paths["manifest.tsv"], rows, COLUMNS)
    print("%s: %s (%s)" % (run, args.status, args.reason))


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    commands = parser.add_subparsers(dest="command", required=True)
    start = commands.add_parser("start", help="prepare the workers and start a campaign")
    start.add_argument("experiment", help="an experiment directory or its experiment.json")
    start.add_argument("--campaign", help="the campaign's name (default: the UTC time, YYYYMMDD-HHMM)")
    start.add_argument("--cluster", default=os.path.join(ROOT, "k8s", "cluster.json"))
    start.add_argument("--recordings-from", help="a campaign whose recordings to replay instead of recording")
    start.add_argument("--prometheus", help="Prometheus URL the runs read (default: the cluster's kube-prometheus-stack)")
    start.add_argument("--allow-dirty", action="store_true", help="run although the working tree has uncommitted changes")
    start.add_argument("--dry-run", action="store_true", help="print each worker's chain and do nothing")
    for name, help_text in (("status", "show each worker's progress"), ("collect", "copy results back and merge the manifest"),
                            ("resume", "restart stopped chains; finished steps are skipped"), ("stop", "stop every chain")):
        command = commands.add_parser(name, help=help_text)
        command.add_argument("campaign")
        if name == "collect":
            command.add_argument("--watch", type=int, default=0, metavar="SECONDS", help="repeat until every chain is done")
        if name == "resume":
            command.add_argument("--prepare", action="store_true", help="ship and build the tree again first")
            command.add_argument("--worker", action="append", help="only this worker (repeatable)")
    mark = commands.add_parser("mark", help="set a run's status")
    mark.add_argument("campaign")
    mark.add_argument("run", help="the run's path in the campaign, such as runs/openclaw/docker/3")
    mark.add_argument("--status", required=True, choices=STATUSES)
    mark.add_argument("--reason", required=True)
    args = parser.parse_args()
    if getattr(args, "campaign", None) and args.command != "start":
        args.campaign = os.path.abspath(args.campaign)
    {"start": cmd_start, "status": cmd_status, "collect": cmd_collect, "resume": cmd_resume,
     "stop": cmd_stop, "mark": cmd_mark}[args.command](args)


if __name__ == "__main__":
    main()
