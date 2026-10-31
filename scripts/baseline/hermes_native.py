#!/usr/bin/env python3
"""Runs the microbenchmark tasks with Hermes alone, using its own Docker terminal.

    python3 scripts/baseline/hermes_native.py --runs 5 --address 10.0.1.3 \\
        --prometheus http://10.106.21.199:9090

The Hermes counterpart of openclaw_native.py: no runner, no bridge, no ARIES
sandbox. The Hermes gateway runs in a container of the image and resources
ARIES gives it, with terminal.backend "docker", and creates its sandbox
container itself through the Docker socket. The configuration is the one ARIES
generated for the same task with only the terminal changed, the task is
submitted to the gateway's /v1/runs API as ARIES submits it, the model is
aries-replay serving the Hermes recordings, and the sandbox runs the task's
image with 2 CPUs, 4 GiB and no disk limit, as ARIES's does.

Results go to runs/ in the layout of an ARIES run and are listed in
.cache/microbench-runs/manifest.tsv with backend "native".
"""

import argparse
import datetime
import glob
import json
import os
import secrets
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from openclaw_native import MANIFEST_HEADER, WARMUP_INSTRUCTION, iso, now, parse_time, prometheus_samples, remove_tree, run, warm_up  # noqa: E402

TASKS = ["mb-noop", "mb-one-call", "mb-ten-calls", "mb-cpu", "mb-output"]
API_PORT = 8642
HERMES_UID = 10000
# Hermes enables its API only with a key of at least 16 characters.
KEY = secrets.token_hex(16)


def hermes_image():
    images = subprocess.run(["docker", "images", "--format", "{{.Repository}}:{{.Tag}}"], capture_output=True, text=True).stdout.split()
    built = sorted(image for image in images if image.startswith("aries-local/hermes:"))
    if not built:
        sys.exit("no aries-local/hermes image; run an ARIES Hermes run on Docker first, which builds it")
    return built[-1]


def config_template(root):
    runs = sorted(glob.glob(os.path.join(root, "runs", "*hermes-microbench-docker-replay*")), key=os.path.getmtime)
    for run_dir in reversed(runs):
        paths = glob.glob(os.path.join(run_dir, "mb-one-call-*", "harness", "config.yaml"))
        if paths:
            return open(paths[0]).read(), paths[0]
    sys.exit("no ARIES Hermes microbenchmark run to take the configuration from; run scripts/microbench/run.sh first")


def native_config(template):
    # The only change: Hermes's own Docker terminal instead of ARIES's SSH
    # bridge, on the task's image with the limits ARIES gives its sandbox.
    head = template.split("\nterminal:")[0].rstrip("\n")
    return head + """
terminal:
    backend: docker
    cwd: /
    timeout: 180
    docker_image: python:3.12.12-slim-bookworm
    container_cpu: 2
    container_memory: 4096
    container_disk: 0
"""


def api(method, path, body=None, timeout=10):
    request = urllib.request.Request("http://127.0.0.1:%d%s" % (API_PORT, path), method=method,
                                     data=json.dumps(body).encode() if body is not None else None,
                                     headers={"Authorization": "Bearer " + KEY, "Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.load(response)


def sandboxes():
    listed = subprocess.run(["docker", "ps", "-a", "--filter", "label=hermes-agent=1", "--format", "{{.Names}}"], capture_output=True, text=True)
    return [name for name in listed.stdout.split() if name]


def created_since(names, since):
    """The sandboxes created at or after since, newest first."""
    found = []
    for name in names:
        inspected = subprocess.run(["docker", "inspect", name, "--format", "{{.Created}}"], capture_output=True, text=True)
        if inspected.returncode != 0:
            continue
        created = parse_time(inspected.stdout.strip())
        if created >= since:
            found.append((created, name))
    return [name for _, name in sorted(found, reverse=True)]


def run_gateway(instruction, session_id):
    """Runs one instruction in the named session, polled every 100 ms as
    ARIES polls it; returns the run's outcome and an error or ""."""
    try:
        admitted = api("POST", "/v1/runs", {"input": instruction, "session_id": session_id})
        run_id = admitted["run_id"]
        while True:
            outcome = api("GET", "/v1/runs/" + run_id)
            if outcome.get("status") in ("completed", "failed", "cancelled", "interrupted"):
                break
            time.sleep(0.1)
        if outcome.get("status") != "completed":
            return outcome, "run %s: %s" % (outcome.get("status"), outcome.get("error", ""))
        return outcome, ""
    except (urllib.error.URLError, OSError, ValueError, KeyError) as problem:
        return {}, "gateway API: %s" % problem


def run_task(args, root, run_dir, task, occurrence, template, image):
    task_id = "%s-%03d" % (task, occurrence)
    # On the node's own disk: the gateway runs as root, which an NFS home
    # directory such as CloudLab's maps to an unprivileged user.
    work = os.path.join("/var/tmp", "aries-baseline", "hermes-" + task_id)
    remove_tree(work)
    os.makedirs(os.path.join(work, "home"))
    with open(os.path.join(work, "home", "config.yaml"), "w") as handle:
        handle.write(native_config(template))
    # Hermes runs as its own user, 10000, as under ARIES, which hands it the
    # staged directory the same way.
    run(["docker", "run", "--rm", "-v", "%s:/w" % work, "busybox:1.37", "chown", "-R", "%d:%d" % (HERMES_UID, HERMES_UID), "/w"])
    instruction = open(os.path.join(root, ".cache", "microbench", "tasks", task, "instruction.md")).read()
    gateway = "hermes-native-" + task_id
    subprocess.run(["docker", "rm", "-f", gateway] + sandboxes(), capture_output=True)
    socket_group = str(os.stat("/var/run/docker.sock").st_gid)
    phases, error = [], ""
    started = now()

    def phase(name, began):
        phases.append({"name": name, "started_at": iso(began), "duration": int((now() - began) * 1e9)})

    # Harness start: until the gateway's API answers, as ARIES waits for it.
    began = now()
    environment = ["HERMES_HOME=" + os.path.join(work, "home"), "HERMES_YOLO_MODE=true", "API_SERVER_HOST=0.0.0.0",
                   "API_SERVER_PORT=%d" % API_PORT, "API_SERVER_KEY=" + KEY, "DEEPSEEK_API_KEY=unused",
                   "TERMINAL_ENV=docker", "TERMINAL_CWD=/", "TERMINAL_TIMEOUT=180",
                   "TERMINAL_DOCKER_IMAGE=python:3.12.12-slim-bookworm", "TERMINAL_CONTAINER_CPU=2",
                   "TERMINAL_CONTAINER_MEMORY=4096", "TERMINAL_CONTAINER_DISK=0", "HERMES_WRITE_SAFE_ROOT=",
                   "HERMES_OTEL_DASHBOARD_LIVE=true", "HERMES_OTEL_DASHBOARD_LIVE_MAX_SPANS=10000",
                   "HERMES_OTEL_DASHBOARD_LIVE_RETENTION_HOURS=0", "HERMES_OTEL_CONTENT_CAPTURE=off"]
    command = ["docker", "run", "-d", "--name", gateway, "-p", "127.0.0.1:%d:%d" % (API_PORT, API_PORT), "--cpus", "2", "--memory", "4g",
               "-u", "%d:%d" % (HERMES_UID, HERMES_UID), "--group-add", socket_group, "-v", "/var/run/docker.sock:/var/run/docker.sock",
               "-v", "/usr/local/bin/docker:/usr/local/bin/docker:ro", "-v", "%s:%s" % (work, work)]
    for entry in environment:
        command += ["-e", entry]
    run(command + ["--entrypoint", "hermes", image, "gateway", "run", "--no-supervise", "--external-supervisor"])
    ready = False
    deadline = now() + 120
    while now() < deadline and not ready:
        state = subprocess.run(["docker", "inspect", gateway, "--format", "{{.State.Running}}"], capture_output=True, text=True)
        if state.stdout.strip() != "true":
            break
        try:
            api("GET", "/v1/models", timeout=2)
            ready = True
        except (urllib.error.URLError, OSError, ValueError):
            time.sleep(0.1)
    phase("harness_start", began)
    reward, outcome, warmups = 0.0, {}, []
    if ready and args.warm:
        # Each warm-up is its own session, as under ARIES.
        began = now()
        warmups, error = warm_up(lambda index: run_gateway(WARMUP_INSTRUCTION, "aries-%s-warmup-%d" % (task_id, index))[1])
        phase("harness_warmup", began)
    if not ready:
        error = "gateway did not start"
    elif not error:
        # The agent's run.
        began = now()
        outcome, error = run_gateway(instruction, "aries-" + task_id)
        phase("harness_run", began)
        # Evaluation, in Hermes's own sandbox: the newest one the task's run
        # created, or else the newest, which a warm-up created and the task
        # reused. A task that ran nothing has none, so its verifier runs in a
        # fresh container of the same image.
        began = now()
        created = created_since(sandboxes(), parse_time(phases[-1]["started_at"])) or sandboxes()
        target = created[0] if created else "hermes-native-eval-" + task_id
        if not created:
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
        if not created:
            subprocess.run(["docker", "rm", "-f", target], capture_output=True)

    sandbox_names = sandboxes()
    harness_dir = os.path.join(run_dir, task_id, "harness")
    os.makedirs(harness_dir, exist_ok=True)
    subprocess.run(["docker", "logs", gateway], stdout=open(os.path.join(harness_dir, "container.log"), "w"), stderr=subprocess.STDOUT)
    with open(os.path.join(harness_dir, "session-outcome.json"), "w") as handle:
        json.dump(outcome, handle, indent=2)

    began = now()
    subprocess.run(["docker", "rm", "-f", gateway] + sandbox_names, capture_output=True)
    phase("cleanup", began)
    finished = now()

    monitor = os.path.join(run_dir, task_id, "monitor")
    os.makedirs(monitor, exist_ok=True)
    time.sleep(3)  # let the last scrape land
    with open(os.path.join(monitor, "resources.jsonl"), "w") as handle:
        for component, names in (("harness", [gateway]), ("sandbox", sandbox_names)):
            for name in names:
                for sample in prometheus_samples(args.prometheus, name, started, finished):
                    sample.update({"component": component, "runtime_name": name})
                    handle.write(json.dumps(sample) + "\n")
    remove_tree(work)
    return {"task_id": task_id, "harness": {"status": "succeeded" if not error else "failed"},
            "evaluation": {"status": "succeeded", "reward": reward}, "phases": phases,
            "duration": int((finished - started) * 1e9), "error": error, "warmups": warmups}


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--runs", type=int, default=1)
    parser.add_argument("--address", required=True, help="this node's IP, where the gateway reaches aries-replay")
    parser.add_argument("--prometheus", required=True)
    parser.add_argument("--port", type=int, default=8090)
    parser.add_argument("--warm", action="store_true", help="send warm-ups through the started gateway before each task")
    parser.add_argument("--tasks", default=",".join(TASKS), help="comma-separated subset of " + ",".join(TASKS))
    args = parser.parse_args()
    args.tasks = args.tasks.split(",")
    if not args.tasks or any(task not in TASKS for task in args.tasks):
        sys.exit("--tasks must name tasks from " + ",".join(TASKS))
    variant = ("warm" if args.warm else "cold") + "-sequential"
    root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    os.chdir(root)

    template, source = config_template(root)
    template = template.replace(":8090/v1", ":%d/v1" % args.port)
    image = hermes_image()
    print("configuration from %s, image %s" % (source, image))
    store = os.path.join(root, ".cache", "microbench-replay", "hermes", "recordings.jsonl")
    if not os.path.exists(store):
        sys.exit(store + " is missing; record Hermes with scripts/microbench/run.sh first")
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    requests = os.path.join(root, ".cache", "microbench-replay", "hermes", "requests-native-replay-%s.jsonl" % stamp)
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
                handle.write(MANIFEST_HEADER)
        for repetition in range(1, args.runs + 1):
            run_dir = os.path.join(root, "runs", "%s-hermes-microbench-native-replay-%s-%d" % (stamp, variant, repetition))
            os.makedirs(run_dir)
            print("==> hermes-microbench-native-replay-%s, run %d of %d" % (variant, repetition, args.runs), flush=True)
            started = now()
            tasks = [run_task(args, root, run_dir, task, index + 1, template, image) for index, task in enumerate(args.tasks)]
            with open(os.path.join(run_dir, "run-result.json"), "w") as handle:
                json.dump({"name": "hermes-microbench-native-replay", "tasks": tasks, "duration": int((now() - started) * 1e9)}, handle, indent=2)
            with open(manifest, "a") as handle:
                handle.write("hermes\tnative\treplay\t%d\t%s\t%s\t%s\n" % (repetition, run_dir, requests, variant))
            print("    %s (%s)" % (run_dir, ", ".join("%s=%s%s" % (t["task_id"], t["evaluation"]["reward"], " " + t["error"][:80] if t["error"] else "") for t in tasks)), flush=True)
    finally:
        server.terminate()
        server.wait()


if __name__ == "__main__":
    main()
