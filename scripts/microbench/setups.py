"""The setups a microbenchmark campaign can compare, in report order.

Each setup has a short name for tables, a longer one for figure legends, a
description for the report, and the scripts/microbench/run.sh arguments that
select it. The Kubernetes variants differ only in how the sandbox pod is made:
under a RuntimeClass, through agent-sandbox, or claimed from a warm pool.
"""

SETUPS = [
    # key, table name, figure name, description
    ("native", "Native", "Native (own Docker)",
     "The harness's own Docker sandbox, with no ARIES involved: the baseline."),
    ("docker", "ARIES Docker", "ARIES + Docker",
     "ARIES creates the sandbox as a Docker container; the harness reaches it through ARIES's tool bridge."),
    ("k8s", "ARIES K8s", "ARIES + Kubernetes",
     "The same, with the harness, sandbox and bridge as Kubernetes pods."),
    ("k8s-gvisor", "K8s gVisor", "ARIES + K8s, gVisor",
     "Kubernetes pods, the sandbox pod under the gVisor RuntimeClass."),
    ("k8s-kata", "K8s Kata", "ARIES + K8s, Kata",
     "Kubernetes pods, the sandbox pod under the Kata Containers RuntimeClass (QEMU)."),
    ("k8s-agent-sandbox", "agent-sandbox", "agent-sandbox, runc",
     "Kubernetes pods, the sandbox created as an agent-sandbox Sandbox, whose controller makes the pod."),
    ("k8s-agent-sandbox-gvisor", "agent-sandbox gVisor", "agent-sandbox, gVisor",
     "An agent-sandbox Sandbox under the gVisor RuntimeClass."),
    ("k8s-agent-sandbox-kata", "agent-sandbox Kata", "agent-sandbox, Kata",
     "An agent-sandbox Sandbox under the Kata RuntimeClass."),
    ("k8s-warm-pool", "Warm pool", "Warm pool, runc",
     "The sandbox claimed from a run-owned agent-sandbox warm pool of one ready pod; weaker isolation."),
    ("k8s-warm-pool-gvisor", "Warm pool gVisor", "Warm pool, gVisor",
     "A warm-pool claim under the gVisor RuntimeClass."),
    ("k8s-warm-pool-kata", "Warm pool Kata", "Warm pool, Kata",
     "A warm-pool claim under the Kata RuntimeClass."),
]
KEYS = [key for key, _, _, _ in SETUPS]
NAME = {key: name for key, name, _, _ in SETUPS}
LABEL = {key: label for key, _, label, _ in SETUPS}
DESCRIPTION = {key: text for key, _, _, text in SETUPS}

# Categorical slots 1-8 of the validated reference palette, in order. A
# figure colours its setups by their position among those it shows, so the
# same campaign always gets the same colours.
PALETTE = ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"]


def ordered(keys):
    """The given setups in report order."""
    return [key for key in KEYS if key in set(keys)]


def colors(keys):
    keys = ordered(keys)
    if len(keys) > len(PALETTE):
        raise ValueError("at most %d setups can be told apart by colour" % len(PALETTE))
    return {key: PALETTE[index] for index, key in enumerate(keys)}


def run_arguments(key):
    """run.sh's --backend and options for a Kubernetes or Docker setup."""
    if key == "docker":
        return ["--backend", "docker"]
    if key == "k8s":
        return ["--backend", "k8s"]
    if not key.startswith("k8s-") or key not in KEYS:
        raise ValueError("no run.sh arguments for setup %r" % key)
    arguments = ["--backend", "k8s"]
    rest = key[len("k8s-"):]
    for mode, flags in (("agent-sandbox", ["--agent-sandbox"]), ("warm-pool", ["--warm-pool", "1"])):
        if rest == mode or rest.startswith(mode + "-"):
            arguments += flags
            rest = rest[len(mode):].lstrip("-")
    if rest:
        arguments += ["--runtime-class", rest]
    return arguments
