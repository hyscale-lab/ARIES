#!/usr/bin/env python3
"""Runs the microbenchmark tasks with OpenClaw alone, using its own Docker sandbox.

    python3 scripts/baseline/openclaw_native.py --runs 5 --address 10.0.1.3 \\
        --prometheus http://10.106.21.199:9090

This is the baseline the ARIES deployments are compared against: no runner,
no bridge, no ARIES sandbox. OpenClaw's gateway runs in a container of the
same image and resources ARIES gives it, creates its sandbox container itself
through the Docker socket (agents.defaults.sandbox.backend "docker"), and runs
every tool there. Everything else is held equal: the configuration is the one
ARIES generated for the same task with only the sandbox and workspace changed,
the model is aries-replay serving the same recordings, and the sandbox runs the
task's image with the same CPU and memory limits and no network.

Each task run is written in the layout of an ARIES run (run-result.json with
phases, the harness's session log, monitor/resources.jsonl from the same
cAdvisor series) and listed in .cache/microbench-runs/manifest.tsv with backend
"native", so scripts/microbench/analyze.py compares it with the ARIES runs.

Run it from an ARIES checkout on the node the comparison measures, with
bin/aries-replay built, the tasks packed, the OpenClaw recordings in
.cache/microbench-replay/openclaw/, and at least one ARIES OpenClaw
microbenchmark run to take the configuration from.
"""

import argparse
import datetime
import glob
import json
import os
import shutil
import socket
import subprocess
import sys
import time
import urllib.parse
import urllib.request

IMAGE = "ghcr.io/openclaw/openclaw:2026.7.1"
TASKS = ["mb-noop", "mb-one-call", "mb-ten-calls", "mb-cpu", "mb-output"]
SANDBOX = "openclaw-sbx-shared"
GATEWAY_PORT = 18789


def run(args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kwargs)


def now():
    return time.time()


def iso(seconds):
    return datetime.datetime.fromtimestamp(seconds, datetime.timezone.utc).isoformat().replace("+00:00", "Z")


def remove_tree(path):
    # The sandbox runs as root and leaves root-owned files in the workspace.
    if os.path.exists(path):
        subprocess.run(["docker", "run", "--rm", "-v", "%s:/target" % path, "busybox:1.37", "rm", "-rf", "/target/home", "/target/ws"],
                       capture_output=True)
        shutil.rmtree(path, ignore_errors=True)


def wait_for_port(container, deadline):
    probe = "require('net').connect(%d,'127.0.0.1').on('connect',()=>process.exit(0)).on('error',()=>process.exit(1))" % GATEWAY_PORT
    while now() < deadline:
        if subprocess.run(["docker", "exec", container, "node", "-e", probe], capture_output=True).returncode == 0:
            return True
        time.sleep(0.25)
    return False


def config_template(root):
    runs = sorted(glob.glob(os.path.join(root, "runs", "*openclaw-microbench-docker-replay")), key=os.path.getmtime)
    for run_dir in reversed(runs):
        paths = glob.glob(os.path.join(run_dir, "mb-one-call-*", "harness", "openclaw.json"))
        if paths:
            with open(paths[0]) as handle:
                return json.load(handle), paths[0]
    sys.exit("no ARIES OpenClaw microbenchmark run to take the configuration from; run scripts/microbench/run.sh first")


def native_config(template, workspace):
    config = json.loads(json.dumps(template))
    defaults = config["agents"]["defaults"]
    defaults["workspace"] = workspace
    # The only change: OpenClaw's own Docker sandbox instead of ARIES's bridge,
    # on the task's image with the limits ARIES gives its sandbox.
    defaults["sandbox"] = {
        "mode": "all", "scope": "shared", "backend": "docker", "workspaceAccess": "rw",
        "docker": {"image": "python:3.12.12-slim-bookworm", "readOnlyRoot": False, "network": "none",
                   "user": "0:0", "cpus": 2, "memory": "4g", "env": {"HOME": "/tmp"}},
    }
    return config


def prometheus_samples(prometheus, name, start, end):
    """Raw cAdvisor samples for one container over a window, as resources.jsonl rows."""
    span = max(1, int(end - start) + 2)
    rows = {}
    for metric, field in (("container_cpu_usage_seconds_total", "cpu"), ("container_memory_working_set_bytes", "memory")):
        query = '%s{name="%s"}[%ds]' % (metric, name, span)
        url = "%s/api/v1/query?%s" % (prometheus.rstrip("/"), urllib.parse.urlencode({"query": query, "time": "%.3f" % (end + 1)}))
        with urllib.request.urlopen(url, timeout=20) as response:
            result = json.load(response)["data"]["result"]
        for series in result:
            for stamp, value in series["values"]:
                stamp = float(stamp)
                if start - 1 <= stamp <= end + 1:
                    rows.setdefault(stamp, {})[field] = float(value)
    samples, previous = [], None
    for stamp in sorted(rows):
        values = rows[stamp]
        if "cpu" not in values or "memory" not in values:
            continue
        cpu_percent = None
        if previous is not None and stamp > previous[0]:
            cpu_percent = 100 * (values["cpu"] - previous[1]) / (stamp - previous[0])
        samples.append({"time": iso(stamp), "second": int(stamp - start), "cpu_usage_nanoseconds": int(values["cpu"] * 1e9),
                        "cpu_percent": cpu_percent, "memory_usage_bytes": int(values["memory"])})
        previous = (stamp, values["cpu"])
    return samples


def run_task(args, root, run_dir, task, occurrence, template):
    task_id = "%s-%03d" % (task, occurrence)
    work = os.path.join(root, ".cache", "baseline-work", task_id)
    remove_tree(work)
    os.makedirs(os.path.join(work, "home"))
    os.makedirs(os.path.join(work, "ws"))
    with open(os.path.join(work, "openclaw.json"), "w") as handle:
        json.dump(native_config(template, os.path.join(work, "ws")), handle, indent=2)
    shutil.copy(os.path.join(root, ".cache", "microbench", "tasks", task, "instruction.md"), os.path.join(work, "task.md"))
    gateway = "openclaw-native-" + task_id
    subprocess.run(["docker", "rm", "-f", gateway, SANDBOX], capture_output=True)
    socket_group = str(os.stat("/var/run/docker.sock").st_gid)
    phases, error = [], ""
    started = now()

    def phase(name, began):
        phases.append({"name": name, "started_at": iso(began), "duration": int((now() - began) * 1e9)})

    # Harness start: until the gateway accepts connections, as ARIES waits for it.
    began = now()
    run(["docker", "run", "-d", "--name", gateway, "--cpus", "2", "--memory", "4g",
         "-u", "%d:%d" % (os.getuid(), os.getgid()), "--group-add", socket_group,
         "-v", "/var/run/docker.sock:/var/run/docker.sock", "-v", "/usr/local/bin/docker:/usr/local/bin/docker:ro",
         "-v", "%s:%s" % (work, work), "-e", "HOME=" + os.path.join(work, "home"),
         "-e", "OPENCLAW_CONFIG_PATH=" + os.path.join(work, "openclaw.json"),
         "-e", "OPENCLAW_GATEWAY_TOKEN=baseline", "-e", "DEEPSEEK_API_KEY=unused",
         "--entrypoint", "openclaw", IMAGE, "gateway", "run", "--port", str(GATEWAY_PORT), "--auth", "token", "--bind", "loopback"])
    ready = wait_for_port(gateway, now() + 120)
    phase("harness_start", began)
    reward = 0.0
    agent = {}
    if not ready:
        error = "gateway did not start"
    else:
        # The agent's run. OpenClaw creates its sandbox container on the first tool call.
        began = now()
        result = subprocess.run(["docker", "exec", gateway, "openclaw", "agent", "--session-key", "agent:main:aries-" + task_id,
                                 "--message-file", os.path.join(work, "task.md"), "--json"], capture_output=True, text=True)
        phase("harness_run", began)
        try:
            agent = json.loads(result.stdout)
        except ValueError:
            error = "agent output: " + (result.stderr or result.stdout)[-300:]
        if result.returncode != 0 and not error:
            error = "agent exited %d: %s" % (result.returncode, result.stderr[-300:])
        # Evaluation, in OpenClaw's own sandbox; a task that ran no tool has none,
        # so its verifier runs in a fresh container of the same image.
        began = now()
        target = SANDBOX
        if subprocess.run(["docker", "inspect", SANDBOX], capture_output=True).returncode != 0:
            target = "openclaw-native-eval-" + task_id
            run(["docker", "run", "-d", "--name", target, "--network", "none", "python:3.12.12-slim-bookworm", "sleep", "600"])
        tests = os.path.join(root, ".cache", "microbench", "tasks", task, "tests")
        run(["docker", "cp", tests, target + ":/tests"])
        subprocess.run(["docker", "exec", target, "sh", "/tests/test.sh"], capture_output=True)
        evaluation = os.path.join(run_dir, task_id, "evaluation")
        os.makedirs(evaluation, exist_ok=True)
        for name in ("reward.txt", "ctrf.json"):
            subprocess.run(["docker", "cp", "%s:/logs/verifier/%s" % (target, name), os.path.join(evaluation, name)], capture_output=True)
        try:
            reward = float(open(os.path.join(evaluation, "reward.txt")).read().strip())
        except (OSError, ValueError):
            error = error or "no verifier reward"
        phase("evaluation", began)
        if target != SANDBOX:
            subprocess.run(["docker", "rm", "-f", target], capture_output=True)

    # Keep the sandbox's identity for the resource query before removing it.
    sandbox_created = None
    inspected = subprocess.run(["docker", "inspect", SANDBOX, "--format", "{{.Created}} {{.State.StartedAt}}"], capture_output=True, text=True)
    if inspected.returncode == 0:
        sandbox_created = inspected.stdout.split()
    harness_dir = os.path.join(run_dir, task_id, "harness")
    os.makedirs(os.path.join(harness_dir, "telemetry"), exist_ok=True)
    for session in glob.glob(os.path.join(work, "home", ".openclaw", "agents", "main", "sessions", "*.jsonl")):
        shutil.copy(session, os.path.join(harness_dir, "telemetry"))
    subprocess.run(["docker", "logs", gateway], stdout=open(os.path.join(harness_dir, "gateway.log"), "w"), stderr=subprocess.STDOUT)
    with open(os.path.join(harness_dir, "agent-result.json"), "w") as handle:
        json.dump(agent, handle, indent=2)

    began = now()
    subprocess.run(["docker", "rm", "-f", gateway, SANDBOX], capture_output=True)
    phase("cleanup", began)
    finished = now()

    monitor = os.path.join(run_dir, task_id, "monitor")
    os.makedirs(monitor, exist_ok=True)
    time.sleep(3)  # let the last scrape land
    with open(os.path.join(monitor, "resources.jsonl"), "w") as handle:
        for component, name in (("harness", gateway), ("sandbox", SANDBOX)):
            for sample in prometheus_samples(args.prometheus, name, started, finished):
                sample.update({"component": component, "runtime_name": name})
                handle.write(json.dumps(sample) + "\n")
    remove_tree(work)
    return {
        "task_id": task_id, "harness": {"status": "succeeded" if not error else "failed"},
        "evaluation": {"status": "succeeded", "reward": reward}, "phases": phases,
        "duration": int((finished - started) * 1e9), "error": error,
        "sandbox_container": sandbox_created,
        "agent_internal_ms": (agent.get("result") or {}).get("meta", {}).get("durationMs"),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--runs", type=int, default=1)
    parser.add_argument("--address", required=True, help="this node's IP, where the gateway reaches aries-replay")
    parser.add_argument("--prometheus", required=True)
    parser.add_argument("--port", type=int, default=8090)
    args = parser.parse_args()
    root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    os.chdir(root)

    template, source = config_template(root)
    template["models"]["providers"]["aries"]["baseUrl"] = "http://%s:%d/v1" % (args.address, args.port)
    print("configuration from " + source)
    store = os.path.join(root, ".cache", "microbench-replay", "openclaw", "recordings.jsonl")
    if not os.path.exists(store):
        sys.exit(store + " is missing; record OpenClaw with scripts/microbench/run.sh first")
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    requests = os.path.join(root, ".cache", "microbench-replay", "openclaw", "requests-native-replay-%s.jsonl" % stamp)
    server = subprocess.Popen(["./bin/aries-replay", "-mode", "replay", "-addr", "0.0.0.0:%d" % args.port, "-store", store,
                               "-log", requests, "-model", "deepseek-flash"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        for _ in range(50):
            try:
                socket.create_connection(("127.0.0.1", args.port), timeout=1).close()
                break
            except OSError:
                time.sleep(0.2)
        manifest = os.path.join(root, ".cache", "microbench-runs", "manifest.tsv")
        if not os.path.exists(manifest):
            with open(manifest, "w") as handle:
                handle.write("harness\tbackend\tmode\trepetition\trun_dir\trequests\n")
        for repetition in range(1, args.runs + 1):
            run_dir = os.path.join(root, "runs", "%s-openclaw-microbench-native-replay-%d" % (stamp, repetition))
            os.makedirs(run_dir)
            print("==> openclaw-microbench-native-replay, run %d of %d" % (repetition, args.runs), flush=True)
            started = now()
            tasks = [run_task(args, root, run_dir, task, index + 1, template) for index, task in enumerate(TASKS)]
            with open(os.path.join(run_dir, "run-result.json"), "w") as handle:
                json.dump({"name": "openclaw-microbench-native-replay", "tasks": tasks, "duration": int((now() - started) * 1e9)}, handle, indent=2)
            with open(manifest, "a") as handle:
                handle.write("openclaw\tnative\treplay\t%d\t%s\t%s\n" % (repetition, run_dir, requests))
            print("    %s (%s)" % (run_dir, ", ".join("%s=%s" % (t["task_id"], t["evaluation"]["reward"]) for t in tasks)), flush=True)
    finally:
        server.terminate()
        server.wait()


if __name__ == "__main__":
    main()
