#!/usr/bin/env python3
"""Phase 0 probe: does a sandbox backend give ARIES's bridge what it needs, and
how long do create, exec and delete take?

    python3 probe.py BACKEND checks
    python3 probe.py BACKEND latency [--iterations 50] [--concurrency 1]

Backends: docker, pod, pod-gvisor, pod-kata, agentsandbox, agentsandbox-gvisor,
agentsandbox-kata, agentsandbox-claim, opensandbox, cube.

Every backend gets the microbenchmark tasks' image and limits (1 CPU, 1 GiB).
Output is one JSON object per line. Phase 0 drives each backend through its
own CLI or SDK, so absolute times include that client's overhead; compare
within a client, and treat the numbers as a go/no-go screen.
"""

import argparse
import concurrent.futures
import hashlib
import json
import os
import shlex
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import uuid

IMAGE = "python:3.12.12-slim-bookworm"
CPUS, MEMORY_MIB = 1, 1024
NAMESPACE = os.environ.get("PROBE_NAMESPACE", "aries-probe")


def run(args, stdin=None, check=True, timeout=300):
    result = subprocess.run(args, input=stdin, capture_output=True, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError("%s: %s" % (" ".join(args[:4]), result.stderr.decode(errors="replace").strip()))
    return result


class Result:
    def __init__(self, stdout, stderr, code):
        self.stdout, self.stderr, self.code = stdout, stderr, code


# --- Docker: the baseline, as ARIES Docker runs a sandbox -----------------

class Docker:
    name = "docker"

    def create(self):
        name = "probe-" + uuid.uuid4().hex[:10]
        run(["docker", "run", "-d", "--name", name, "--cpus", str(CPUS), "--memory", "%dm" % MEMORY_MIB,
             "--entrypoint", "sleep", IMAGE, "infinity"])
        return name

    def exec(self, sandbox, argv, stdin=None, timeout=300):
        r = run(["docker", "exec", "-i", sandbox] + argv, stdin=stdin, check=False, timeout=timeout)
        return Result(r.stdout, r.stderr, r.returncode)

    def exec_cancel(self, sandbox, argv, after):
        process = subprocess.Popen(["docker", "exec", "-i", sandbox] + argv, stdin=subprocess.PIPE,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        time.sleep(after)
        process.kill()
        process.wait()

    def upload(self, sandbox, local, remote):
        run(["docker", "cp", local, "%s:%s" % (sandbox, remote)])

    def download(self, sandbox, remote, local):
        r = run(["docker", "cp", "%s:%s" % (sandbox, remote), local], check=False)
        return r.returncode == 0, r.stderr.decode(errors="replace").strip()

    def delete(self, sandbox):
        run(["docker", "rm", "-f", sandbox])

    def absent(self, sandbox):
        return run(["docker", "inspect", sandbox], check=False).returncode != 0


# --- Kubernetes: a plain pod, or one made by agent-sandbox ----------------

def pod_spec(runtime_class):
    spec = {"restartPolicy": "Never", "terminationGracePeriodSeconds": 0,
            "containers": [{"name": "sandbox", "image": IMAGE, "imagePullPolicy": "IfNotPresent",
                            "command": ["sleep", "infinity"],
                            "resources": {"limits": {"cpu": str(CPUS), "memory": "%dMi" % MEMORY_MIB},
                                          "requests": {"cpu": str(CPUS), "memory": "%dMi" % MEMORY_MIB}}}]}
    if runtime_class:
        spec["runtimeClassName"] = runtime_class
    if os.environ.get("PROBE_NODE"):
        spec["nodeSelector"] = {"kubernetes.io/hostname": os.environ["PROBE_NODE"]}
    return spec


class Kube:
    """Exec and transfer through kubectl into a pod, as ARIES's K8s backend
    does through the API."""

    def exec(self, sandbox, argv, stdin=None, timeout=300):
        r = run(["kubectl", "-n", NAMESPACE, "exec", "-i", self.pod(sandbox), "-c", "sandbox", "--"] + argv,
                stdin=stdin, check=False, timeout=timeout)
        return Result(r.stdout, r.stderr, r.returncode)

    def exec_cancel(self, sandbox, argv, after):
        process = subprocess.Popen(["kubectl", "-n", NAMESPACE, "exec", "-i", self.pod(sandbox), "-c", "sandbox", "--"]
                                   + argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        time.sleep(after)
        process.kill()
        process.wait()

    def upload(self, sandbox, local, remote):
        with open(local, "rb") as handle:
            data = handle.read()
        r = self.exec(sandbox, ["/bin/sh", "-c", "cat > " + shlex.quote(remote)], stdin=data)
        if r.code:
            raise RuntimeError(r.stderr.decode())

    def download(self, sandbox, remote, local):
        r = self.exec(sandbox, ["/bin/cat", remote])
        if r.code:
            return False, r.stderr.decode(errors="replace").strip()
        with open(local, "wb") as handle:
            handle.write(r.stdout)
        return True, ""

    def pod(self, sandbox):
        return sandbox


class Pod(Kube):
    def __init__(self, runtime_class=None):
        self.runtime_class = runtime_class
        self.name = "pod" + ("-" + runtime_class if runtime_class else "")

    def create(self):
        name = "probe-" + uuid.uuid4().hex[:10]
        manifest = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": NAMESPACE},
                    "spec": pod_spec(self.runtime_class)}
        run(["kubectl", "apply", "-f", "-"], stdin=json.dumps(manifest).encode())
        run(["kubectl", "-n", NAMESPACE, "wait", "pod/" + name, "--for=condition=Ready", "--timeout=300s"])
        return name

    def delete(self, sandbox):
        run(["kubectl", "-n", NAMESPACE, "delete", "pod", sandbox, "--wait=true", "--grace-period=0"])

    def absent(self, sandbox):
        return run(["kubectl", "-n", NAMESPACE, "get", "pod", sandbox], check=False).returncode != 0


class AgentSandbox(Kube):
    """A Sandbox custom resource; its controller makes the pod."""

    def __init__(self, runtime_class=None):
        self.runtime_class = runtime_class
        self.name = "agentsandbox" + ("-" + runtime_class if runtime_class else "")
        self.pods = {}

    def create(self):
        name = "probe-" + uuid.uuid4().hex[:10]
        manifest = {"apiVersion": "agents.x-k8s.io/v1beta1", "kind": "Sandbox",
                    "metadata": {"name": name, "namespace": NAMESPACE},
                    "spec": {"podTemplate": {"spec": pod_spec(self.runtime_class)}}}
        run(["kubectl", "apply", "-f", "-"], stdin=json.dumps(manifest).encode())
        run(["kubectl", "-n", NAMESPACE, "wait", "sandbox/" + name, "--for=condition=Ready", "--timeout=300s"])
        return name

    def pod(self, sandbox):
        if sandbox not in self.pods:
            self.pods[sandbox] = find_pod(sandbox)
        return self.pods[sandbox]

    def delete(self, sandbox):
        pod = self.pod(sandbox)
        run(["kubectl", "-n", NAMESPACE, "delete", "sandbox", sandbox, "--wait=true"])
        run(["kubectl", "-n", NAMESPACE, "wait", "pod/" + pod, "--for=delete", "--timeout=300s"], check=False)

    def absent(self, sandbox):
        return (run(["kubectl", "-n", NAMESPACE, "get", "sandbox", sandbox], check=False).returncode != 0 and
                run(["kubectl", "-n", NAMESPACE, "get", "pod", self.pods.get(sandbox, sandbox)],
                    check=False).returncode != 0)


def find_pod(owner):
    """The pod a Sandbox owns, by owner reference."""
    pods = json.loads(run(["kubectl", "-n", NAMESPACE, "get", "pods", "-o", "json"]).stdout)["items"]
    for pod in pods:
        for ref in pod["metadata"].get("ownerReferences", []):
            if ref.get("kind") == "Sandbox" and ref.get("name") == owner:
                return pod["metadata"]["name"]
    if any(p["metadata"]["name"] == owner for p in pods):
        return owner
    raise RuntimeError("no pod for sandbox " + owner)


class AgentSandboxClaim(AgentSandbox):
    """A claim on a pre-warmed pool (SandboxTemplate + SandboxWarmPool,
    created by `probe.py agentsandbox-claim pool`)."""

    name = "agentsandbox-claim"
    POOL = "probe-pool"

    def __init__(self):
        super().__init__(None)
        self.name = "agentsandbox-claim"

    def ensure_pool(self, replicas):
        template = {"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxTemplate",
                    "metadata": {"name": self.POOL, "namespace": NAMESPACE},
                    "spec": {"podTemplate": {"spec": pod_spec(None)}}}
        pool = {"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxWarmPool",
                "metadata": {"name": self.POOL, "namespace": NAMESPACE},
                "spec": {"replicas": replicas, "sandboxTemplateRef": {"name": self.POOL}}}
        for manifest in (template, pool):
            run(["kubectl", "apply", "-f", "-"], stdin=json.dumps(manifest).encode())

    def create(self):
        name = "probe-" + uuid.uuid4().hex[:10]
        claim = {"apiVersion": "extensions.agents.x-k8s.io/v1beta1", "kind": "SandboxClaim",
                 "metadata": {"name": name, "namespace": NAMESPACE},
                 "spec": {"warmPoolRef": {"name": self.POOL}, "lifecycle": {"shutdownPolicy": "Delete"}}}
        run(["kubectl", "apply", "-f", "-"], stdin=json.dumps(claim).encode())
        deadline = time.time() + 300
        while time.time() < deadline:
            status = json.loads(run(["kubectl", "-n", NAMESPACE, "get", "sandboxclaim", name, "-o", "json"]).stdout
                                ).get("status", {})
            ready = [c for c in status.get("conditions", []) if c.get("type") == "Ready"]
            if ready and ready[0].get("status") == "True":
                self.claims = getattr(self, "claims", {})
                self.claims[name] = status
                return name
            time.sleep(0.05)
        raise RuntimeError("claim %s not ready: %s" % (name, status))

    def pod(self, sandbox):
        if sandbox not in self.pods:
            status = self.claims.get(sandbox, {})
            target = (status.get("sandbox") or {}).get("name") or (status.get("sandboxStatus") or {}).get("name") \
                or sandbox
            self.pods[sandbox] = find_pod(target)
        return self.pods[sandbox]

    def delete(self, sandbox):
        pod = self.pod(sandbox)
        run(["kubectl", "-n", NAMESPACE, "delete", "sandboxclaim", sandbox, "--wait=true"])
        run(["kubectl", "-n", NAMESPACE, "wait", "pod/" + pod, "--for=delete", "--timeout=300s"], check=False)

    def absent(self, sandbox):
        return run(["kubectl", "-n", NAMESPACE, "get", "pod", self.pods.get(sandbox, sandbox)],
                   check=False).returncode != 0


BACKENDS = {
    "docker": Docker,
    "pod": lambda: Pod(),
    "pod-gvisor": lambda: Pod("gvisor"),
    "pod-kata": lambda: Pod("kata"),
    "agentsandbox": lambda: AgentSandbox(),
    "agentsandbox-gvisor": lambda: AgentSandbox("gvisor"),
    "agentsandbox-kata": lambda: AgentSandbox("kata"),
    "agentsandbox-claim": AgentSandboxClaim,
}

try:
    import probe_api  # noqa: E402  Cube and OpenSandbox adapters, kept apart for their SDKs
    BACKENDS.update(probe_api.BACKENDS)
except ImportError:
    pass


# --- Checks against what the bridge needs ----------------------------------

def checks(backend):
    out = {"backend": backend.name, "image": IMAGE}
    began = time.time()
    sandbox = backend.create()
    out["create_s"] = round(time.time() - began, 3)
    try:
        argv = ["/usr/bin/printf", "%s|", "a b", "$HOME", "'q'", ""]
        r = backend.exec(sandbox, argv)
        out["exact_argv"] = r.stdout == b"a b|$HOME|'q'||"
        r = backend.exec(sandbox, ["/usr/bin/printf", "\\377\\000x"])
        out["stdout_binary"] = r.stdout == b"\xff\x00x"
        r = backend.exec(sandbox, ["/bin/cat"], stdin=b"hello\x00world\n")
        out["stdin_binary"] = r.stdout == b"hello\x00world\n"
        r = backend.exec(sandbox, ["/bin/sh", "-c", "echo out; echo err >&2; exit 7"])
        # kubectl adds its own "command terminated" line; the API does not.
        stderr = r.stderr.split(b"command terminated with exit code")[0]
        out["stdout_stderr_split"] = r.stdout == b"out\n" and stderr == b"err\n"
        out["exit_code"] = r.code
        out["exit_code_ok"] = r.code == 7
        r = backend.exec(sandbox, ["/bin/sh", "-c", "nproc; cat /sys/fs/cgroup/cpu.max 2>/dev/null; "
                                   "cat /sys/fs/cgroup/memory.max 2>/dev/null; "
                                   "awk '/MemTotal/{print $2}' /proc/meminfo; uname -r"])
        out["inside_limits"] = r.stdout.decode(errors="replace").split()
        backend.exec_cancel(sandbox, ["/bin/sh", "-c", "sleep 30; echo late"], after=1.0)
        time.sleep(1)
        r = backend.exec(sandbox, ["/usr/local/bin/python3", "-c",
                                   "import glob\nn=0\nfor p in glob.glob('/proc/[0-9]*/cmdline'):\n"
                                   "    try: n+=open(p,'rb').read()==b'sleep\\x0030\\x00'\n"
                                   "    except OSError: pass\nprint(n)"])
        out["cancel_leaves_command_running"] = r.stdout.decode().strip()
        with tempfile.TemporaryDirectory() as work:
            local = os.path.join(work, "up.bin")
            data = os.urandom(1 << 20)
            with open(local, "wb") as handle:
                handle.write(data)
            backend.upload(sandbox, local, "/tmp/up.bin")
            r = backend.exec(sandbox, ["/usr/bin/sha256sum", "/tmp/up.bin"])
            out["upload_ok"] = r.stdout.split()[:1] == [hashlib.sha256(data).hexdigest().encode()]
            ok, _ = backend.download(sandbox, "/tmp/up.bin", os.path.join(work, "down.bin"))
            out["download_ok"] = ok and open(os.path.join(work, "down.bin"), "rb").read() == data
            ok, message = backend.download(sandbox, "/tmp/missing", os.path.join(work, "missing"))
            out["missing_file_reported"] = (not ok, message[:120])
    finally:
        began = time.time()
        backend.delete(sandbox)
        out["delete_s"] = round(time.time() - began, 3)
        out["absent_after_delete"] = backend.absent(sandbox)
    print(json.dumps(out), flush=True)


# --- Latency: create, first exec, ten execs, delete ------------------------

def one(backend):
    times = {}
    began = time.time()
    sandbox = backend.create()
    times["create_s"] = time.time() - began
    try:
        began = time.time()
        backend.exec(sandbox, ["/bin/echo", "hi"])
        times["first_exec_s"] = time.time() - began
        execs = []
        for _ in range(10):
            began = time.time()
            backend.exec(sandbox, ["/bin/echo", "hi"])
            execs.append(time.time() - began)
        times["exec_median_s"] = statistics.median(execs)
    finally:
        began = time.time()
        backend.delete(sandbox)
        times["delete_s"] = time.time() - began
        times["absent"] = backend.absent(sandbox)
    return times


def latency(backend, iterations, concurrency):
    lock = threading.Lock()
    rows = []

    def task(_):
        try:
            row = one(backend)
        except Exception as error:  # a failure is a result, not a crash
            row = {"error": str(error)[:300]}
        with lock:
            rows.append(row)
            print(json.dumps({"backend": backend.name, "concurrency": concurrency, **row}), flush=True)

    with concurrent.futures.ThreadPoolExecutor(concurrency) as pool:
        list(pool.map(task, range(iterations)))
    ok = [r for r in rows if "error" not in r]
    summary = {"backend": backend.name, "concurrency": concurrency, "iterations": iterations,
               "failures": len(rows) - len(ok)}
    for key in ("create_s", "first_exec_s", "exec_median_s", "delete_s"):
        values = sorted(r[key] for r in ok)
        if values:
            summary[key] = {"p50": round(statistics.median(values), 3),
                            "p95": round(values[min(len(values) - 1, int(len(values) * 0.95))], 3),
                            "mean": round(statistics.fmean(values), 3)}
    summary["all_absent"] = all(r.get("absent") for r in ok)
    print(json.dumps({"summary": summary}), flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("backend", choices=sorted(BACKENDS))
    parser.add_argument("action", choices=["checks", "latency", "pool"])
    parser.add_argument("--iterations", type=int, default=50)
    parser.add_argument("--concurrency", type=int, default=1)
    parser.add_argument("--replicas", type=int, default=10)
    args = parser.parse_args()
    backend = BACKENDS[args.backend]()
    if args.action == "checks":
        checks(backend)
    elif args.action == "pool":
        backend.ensure_pool(args.replicas)
    else:
        latency(backend, args.iterations, args.concurrency)


if __name__ == "__main__":
    sys.exit(main())
