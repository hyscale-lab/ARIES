#!/usr/bin/env python3
"""Bounded, one-attempt Terminal-Bench page collection on a prepared worker.

Prepare is offline: it writes profiles and immutable experiment choices only.
Run performs setup before starting the per-task observer, then invokes ARIES
once. SIGTERM/SIGINT stop new admissions and let the native lifecycle finish.
"""
import argparse
import collections
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import signal
import socket
import subprocess
import sys
import time
import tomllib
import urllib.error
import urllib.request

REPO = Path(__file__).resolve().parents[2]
DEFAULT_ROOT = REPO / "runs/tsb-tool-pages-suite-001"
TEMPLATE = REPO / "profiles/experiments/tsb-tool-pages/fix-git.json"
VERSIONS = REPO / "configs/versions.json"
DATASET = REPO / ".cache/terminal-bench-2-1"
ARTIFACT_OWNER = None
SCHEMA = "aries.tsb.page-suite.v1"
EXPECTED_TASKS = 89


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def atomic_json(path, value):
    path = Path(path)
    temporary = path.with_name("." + path.name + ".tmp")
    with temporary.open("w") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
        set_owner(stream.fileno())
    os.replace(temporary, path)
    directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def capture(args, cwd=REPO):
    return subprocess.check_output(args, cwd=cwd, text=True, stderr=subprocess.PIPE).strip()


def inside_repo(path):
    path = Path(path).resolve()
    if not path.is_relative_to(REPO) or path == REPO:
        raise ValueError("suite artifacts must be inside this repository")
    return path


def runtime_files():
    files = {REPO / "bin" / name for name in ("aries", "aries-bridge", "aries-echo", "aries-ssh-client")}
    files.update((TEMPLATE, VERSIONS))
    files.update(Path(__file__).parent / name for name in (
        "suite.py", "suite_report.py", "overhead.py", "serve.py", "batches.py", "compare.py",
        "boundaries.py", "vmm_observer.py", "model_occurrences.py"))
    for name in ("tsb-companion", "tsb-access-dirty-plugin"):
        files.update(p for p in (REPO / "scripts" / name).rglob("*") if p.is_file()
                     and not any(part in ("node_modules", "__pycache__", ".git") for part in p.parts))
    return {str(p.relative_to(REPO)): digest(p) for p in sorted(files)}


def source_record():
    dirty = capture(["git", "diff", "--name-only", "HEAD"]).splitlines()
    difference = subprocess.check_output(["git", "diff", "--binary", "HEAD"], cwd=REPO)
    return {
        "repository": str(REPO),
        "revision": capture(["git", "rev-parse", "HEAD"]),
        "tracked_changes": dirty,
        "tracked_diff_sha256": hashlib.sha256(difference).hexdigest(),
        "runtime_files_sha256": runtime_files(),
    }


def task_catalog(shard_count):
    pins = json.loads(VERSIONS.read_text())
    revision = capture(["git", "rev-parse", "HEAD"], cwd=DATASET)
    if revision != pins["terminalbench2"]["revision"]:
        raise ValueError("dataset HEAD differs from configs/versions.json pin")
    if capture(["git", "status", "--porcelain=v1", "--untracked-files=all"], cwd=DATASET):
        raise ValueError("dataset must be clean before preparing an exact suite")
    paths = sorted((DATASET / "tasks").glob("*/task.toml"))
    if len(paths) != EXPECTED_TASKS:
        raise ValueError("expected exactly 89 native task directories")
    records = []
    for index, path in enumerate(paths):
        task = tomllib.loads(path.read_text())
        task_id = path.parent.name
        if not re.fullmatch(r"[a-z0-9][a-z0-9._-]*", task_id):
            raise ValueError("unsafe task ID: " + task_id)
        execution_id = task_id + "-001"
        hint = re.sub(r"[^a-z0-9]+", "-", execution_id.lower())[:24].rstrip("-")
        environment = task["environment"]
        for key in ("cpus", "memory_mb", "storage_mb", "gpus"):
            if key not in environment:
                raise ValueError("native environment field missing: " + task_id + "/" + key)
        records.append({
            "task_id": task_id,
            "execution_task_id": execution_id,
            "catalog_index": index,
            "shard_index": index % shard_count,
            "task_toml": str(path),
            "task_toml_sha256": digest(path),
            "native_environment": {key: environment[key] for key in (
                "docker_image", "cpus", "memory_mb", "storage_mb", "gpus", "allow_internet", "build_timeout_sec")},
            "native_agent_timeout_sec": task["agent"]["timeout_sec"],
            "native_verifier_timeout_sec": task["verifier"]["timeout_sec"],
            "vm_prefix": "aries-sandbox-" + hint + "-",
        })
    if len({record["vm_prefix"] for record in records}) != len(records):
        raise ValueError("native runtime prefix hints collide; require exact disambiguation")
    return pins, records


def profile_for(record, root, port):
    profile = json.loads(TEMPLATE.read_text())
    profile["name"] = "tsb-tool-pages-" + record["task_id"]
    profile["versions_file"] = str(VERSIONS)
    profile["overrides_file"] = ""
    profile["execution"] = {"concurrency": 1}
    profile["benchmark"]["root"] = str(DATASET)
    profile["benchmark"]["tasks"] = [record["task_id"]]
    profile["harness"]["step_observation"]["observer_url"] = "http://172.17.0.1:" + str(port)
    profile["harness"]["step_observation"]["plugin_dir"] = str(REPO / "scripts/tsb-access-dirty-plugin")
    # Task environment CPU/RAM are passed by the native benchmark, with no override.
    profile["sandbox"]["deployment"]["vhive"].pop("vcpus", None)
    profile["sandbox"]["deployment"]["vhive"].pop("memory_mb", None)
    profile["output_dir"] = str(root / record["task_id"] / "runs")
    return profile


def selection_for(path, records):
    """Explicit fresh-attempt selection; never infer a paid retry."""
    catalog_ids = {r["task_id"] for r in records}
    if path is None:
        return {"reason": "initial complete catalog", "task_ids": sorted(catalog_ids),
                "source_path": None, "source_sha256": None}
    path = inside_repo(path)
    selection = json.loads(path.read_text())
    ids = selection.get("task_ids")
    if not isinstance(ids, list) or any(not isinstance(x, str) for x in ids):
        raise ValueError("selection task_ids must be a list of strings")
    if len(ids) != len(set(ids)) or not set(ids) <= catalog_ids:
        raise ValueError("selection contains duplicate or unknown task IDs")
    if not isinstance(selection.get("reason"), str) or not selection["reason"].strip():
        raise ValueError("selection requires a concrete reason")
    return {**selection, "source_path": str(path), "source_sha256": digest(path)}


def reconcile_terminal_censor(measurement, comparison, audit, audit_sha256):
    """Preserve errors while qualifying only independently audited complete windows."""
    if (audit.get("status") != "censored"
            or measurement.get("completed_window_evidence_valid") is not True
            or comparison.get("status") != "succeeded"
            or not audit.get("censored_group_ids")
            or "primary_metric_group_ids" not in audit):
        return
    measurement.update(
        pre_audit_status=measurement["status"], status="censored",
        protocol_complete_llm_steps=len(audit["primary_metric_group_ids"]),
        censored_llm_steps=len(audit["censored_group_ids"]),
        censored_groups_without_comparison=audit.get("censored_groups_without_comparison", []),
        terminal_censor_audit_sha256=audit_sha256,
        terminal_censor_reason=audit.get("censoring_reason"))


def selected_records(manifest):
    ids = manifest["selected_task_ids"]
    if len(ids) != len(set(ids)):
        raise ValueError("duplicate selected task IDs")
    records = [r for r in manifest["tasks"]
               if r["shard_index"] == manifest["shard_index"] and r["task_id"] in ids]
    if {r["task_id"] for r in records} != set(ids):
        raise ValueError("selected task is unknown or assigned to another worker")
    return records



RSTAN_OBSERVER_ALLOWANCE = {
    "metadata_barrier_timeout_seconds": 300,
    "publication_timeout_seconds": 300,
    "http_timeout_seconds": 900,
    "hook_timeout_seconds": 905,
}
OBSERVER_PLUGIN_FILES = ("index.mjs", "end-request.mjs", "openclaw.plugin.json", "package.json")


def rstan_observer_allowance(config):
    allowance = config.get("rstan_observer_allowance")
    if allowance is not None and allowance != RSTAN_OBSERVER_ALLOWANCE:
        raise ValueError("only the reviewed RStan observer timeout allowance is supported")
    return allowance


def prepare_observer_override(record, profile, config, task_root):
    allowance = rstan_observer_allowance(config)
    if record["task_id"] != "rstan-to-pystan" or allowance is None:
        return None
    plugin = task_root / "observer-plugin"
    plugin.mkdir(exist_ok=True)
    base = REPO / "scripts/tsb-access-dirty-plugin"
    base_hashes = {}
    for name in OBSERVER_PLUGIN_FILES:
        source = base / name
        data = source.read_bytes()
        base_hashes[str(source)] = digest(source)
        if name == "index.mjs":
            for old, new, count in ((b"timeout:125000", b"timeout:905000", 1),
                                    (b"timeoutMs:125000", b"timeoutMs:905000", 2),
                                    (b"AbortSignal.timeout(120000)", b"AbortSignal.timeout(900000)", 1)):
                if data.count(old) != count:
                    raise ValueError("reviewed plugin timeout template changed")
                data = data.replace(old, new)
        elif name == "end-request.mjs":
            if data.count(b"AbortSignal.timeout(120000)") != 1:
                raise ValueError("reviewed helper timeout template changed")
            data = data.replace(b"AbortSignal.timeout(120000)", b"AbortSignal.timeout(900000)")
        (plugin / name).write_bytes(data)
    selected = dict(config)
    selected.pop("rstan_observer_allowance", None)
    selected.update({key: allowance[key] for key in
                     ("metadata_barrier_timeout_seconds", "publication_timeout_seconds")})
    selected["observer_http_timeout_seconds"] = allowance["http_timeout_seconds"]
    selected["observer_hook_timeout_seconds"] = allowance["hook_timeout_seconds"]
    config_path = task_root / "observer-config.json"
    atomic_json(config_path, selected)
    profile["harness"]["step_observation"]["plugin_dir"] = str(plugin)
    return {"config": {"path": str(config_path), "sha256": digest(config_path)},
            "plugin_dir": str(plugin),
            "plugin_files_sha256": {str(plugin / name): digest(plugin / name) for name in OBSERVER_PLUGIN_FILES},
            "base_plugin_files_sha256": base_hashes,
            "allowance": allowance,
            "scope": "RStan observer waits only; native agent/verifier deadlines unchanged"}


def validate_observer_override(record):
    binding = record.get("observer_override")
    if not binding:
        return None
    if record["task_id"] != "rstan-to-pystan" or binding["allowance"] != RSTAN_OBSERVER_ALLOWANCE:
        raise ValueError("observer timeout override exceeds the reviewed RStan scope")
    expected_files = {str(Path(binding["plugin_dir"]) / name) for name in OBSERVER_PLUGIN_FILES}
    if set(binding["plugin_files_sha256"]) != expected_files:
        raise ValueError("observer plugin file binding incomplete")
    for name, expected in {**binding["base_plugin_files_sha256"], **binding["plugin_files_sha256"]}.items():
        if digest(name) != expected:
            raise ValueError("observer plugin changed after preparation: " + name)
    config = binding["config"]
    if digest(config["path"]) != config["sha256"]:
        raise ValueError("per-task observer config changed after preparation")
    selected = read_json(config["path"])
    if any(selected.get(key) != RSTAN_OBSERVER_ALLOWANCE[key] for key in
           ("metadata_barrier_timeout_seconds", "publication_timeout_seconds")):
        raise ValueError("per-task observer timeout binding mismatch")
    return config["path"]

def prepare(args):
    root = inside_repo(args.root)
    config = read_vmm_config(getattr(args, "access_dirty_config", None))
    root.mkdir(parents=True, exist_ok=True)
    state_dir = root / "state"
    state_dir.mkdir(exist_ok=True)
    manifest_dir = root / "manifests"
    manifest_dir.mkdir(exist_ok=True)
    existing = state_dir / ("shard-" + str(args.shard_index) + ".json")
    manifest_path = manifest_dir / ("shard-" + str(args.shard_index) + ".json")
    if existing.exists() and json.loads(existing.read_text()).get("status") != "prepared":
        raise ValueError("this suite has already started; prepare cannot reset attempt records")
    pins, records = task_catalog(args.shard_count)
    selection = selection_for(getattr(args, "selection_file", None), records)
    selected = [r for r in records if r["shard_index"] == args.shard_index
                and r["task_id"] in selection["task_ids"]]
    rstan_observer_allowance(config)
    detailed = config.get("detailed_task_ids", ["fix-git", "prove-plus-comm", "overfull-hbox", "rstan-to-pystan", "schemelike-metacircular-eval"])
    if len(detailed) != len(set(detailed)) or not set(detailed) <= {r["task_id"] for r in records}:
        raise ValueError("invalid detailed task selection")
    required_detail = {"fix-git", "prove-plus-comm", "overfull-hbox", "rstan-to-pystan", "schemelike-metacircular-eval"}
    if set(detailed) != required_detail:
        raise ValueError("detailed attribution is scoped to the five approved tasks")
    if any(r["task_id"] in required_detail for r in selected) and not getattr(args, "attribution_config", None):
        raise ValueError("selected detailed tasks require attribution config")
    config["detailed_task_ids"] = detailed
    agent_timeouts = config.get("agent_timeout_overrides", {"rstan-to-pystan": 5400})
    catalog_ids = {record["task_id"] for record in records}
    if (not isinstance(agent_timeouts, dict)
            or any(task not in catalog_ids for task in agent_timeouts)
            or any(type(seconds) is not int or seconds <= 0
                   for seconds in agent_timeouts.values())):
        raise ValueError("agent_timeout_overrides requires catalog task IDs and positive integer seconds")
    config["agent_timeout_overrides"] = agent_timeouts
    companion = inside_repo(config.get("companion_path", REPO / "scripts/tsb-companion"))
    config["companion_path"] = str(companion)
    source_files = {str(p): digest(p) for p in companion.rglob("*")
                    if p.is_file() and "__pycache__" not in p.parts}
    if not (companion / "capture.py").exists() or not (companion / "epochs.py").exists():
        raise ValueError("VMM companion source is incomplete")
    config["companion_source_hashes"] = source_files
    config["pause_policy"] = "pause_during_llm"
    target = root / "access-dirty-config.json"
    atomic_json(target, config)
    access_dirty = {"path": str(target), "sha256": digest(target), "detailed_task_ids": detailed}
    for record in selected:
        task_root = root / record["task_id"]
        task_root.mkdir(exist_ok=True)
        status_path = task_root / "status.json"
        if status_path.exists():
            status = json.loads(status_path.read_text())
            if status.get("status") != "pending" or status.get("paid_attempts", 0):
                raise ValueError("refusing to replace a non-pending task: " + record["task_id"])
        profile_path = task_root / "profile.json"
        profile = profile_for(record, root, args.port)
        record["detailed_attribution"] = record["task_id"] in access_dirty["detailed_task_ids"]
        if record["task_id"] in config["agent_timeout_overrides"]:
            agent_timeout = config["agent_timeout_overrides"][record["task_id"]]
            override = task_root / "runtime-overrides.json"
            atomic_json(override, {"agent_timeout_seconds": agent_timeout})
            profile["overrides_file"] = str(override)
            record["runtime_overrides"] = {"path": str(override), "sha256": digest(override),
                                           "agent_timeout_seconds": agent_timeout,
                                           "verifier_timeout_seconds": record["native_verifier_timeout_sec"]}
        binding = prepare_observer_override(record, profile, config, task_root)
        if binding is not None:
            record["observer_override"] = binding
        atomic_json(profile_path, profile)
        record["profile"] = str(profile_path)
        record["profile_sha256"] = digest(profile_path)
        atomic_json(status_path, {
            "schema": SCHEMA, "task_id": record["task_id"],
            "execution_task_id": record["execution_task_id"],
            "status": "pending", "paid_attempts": 0, "updated_at": utc(),
            "profile": str(profile_path),
        })
    attribution = None
    if getattr(args, "attribution_config", None):
        config = json.loads(inside_repo(args.attribution_config).read_text())
        target = root / "attribution-config.json"
        atomic_json(target, config)
        inputs = {}
        for key in ("layout_path", "kernel_image", "names_layout_path", "owner_layout_path"):
            if key not in config:
                continue
            if config.get(key):
                source = inside_repo(config[key])
                inputs[key] = {"path": str(source), "sha256": digest(source)}
        attribution = {"path": str(target), "sha256": digest(target), "inputs": inputs}
    manifest = {
        "schema": SCHEMA, "prepared_at": utc(), "hostname": socket.getfqdn(),
        "root": str(root), "task_count": len(records),
        "shard_index": args.shard_index, "shard_count": args.shard_count,
        "shard_task_count": len(selected),
        "selected_task_ids": [r["task_id"] for r in selected],
        "selection": selection,
        "execution": {"concurrency_per_worker": 1, "observer_port": args.port,
                      "min_free_gib": args.min_free_gib, "artifact_owner": args.owner,
                      "observation_mode": args.observation_mode,
                      "attribution_config": attribution, "access_dirty_config": access_dirty,
                      "attempts_per_task": 1, "automatic_paid_retries": 0},
        "budget": {"task_attempts_entire_catalog": len(records),
                   "task_attempts_this_shard": len(selected),
                   "per_task_agent_timeout": "native task.toml [agent].timeout_sec",
                   "per_task_verifier_timeout": "native task.toml [verifier].timeout_sec",
                   "token_limit": "unspecified; native harness/model defaults",
                   "automatic_paid_retries": 0},
        "model": json.loads(TEMPLATE.read_text())["model"],
        "runtime": json.loads(TEMPLATE.read_text())["runtime"],
        "source": source_record(),
        "dataset": {"path": str(DATASET),
                    "repository_url": pins["terminalbench2"]["repository_url"],
                    "revision": pins["terminalbench2"]["revision"], "clean": True},
        "versions": pins, "versions_sha256": digest(VERSIONS), "tasks": records,
        "method": {
            "setup_before_observer": True, "native_resources_and_timeouts": True,
            "one_native_run_per_task": True, "stop_admission_on_cleanup_uncertainty": True,
            "signals": "stop admission and wait for native active task cleanup; no forced kill",
            "measurement": "LLM tool-request batch through all expected native results ready for the LLM",
            "observation_unit": "llm_reasoning_step",
            "measurement_valid_scope": "expected_batch_registration_and_result_completion_only",
            "coverage_complete": False,
            "native_timing_boundary": "complete assistant tool-request declaration before harness preparation through all expected native results ready for the LLM",
        },
    }
    manifest["method"]["pause_policy"] = "pause_during_llm_including_initial_request"
    manifest["method"]["native_resources_and_timeouts"] = False
    manifest["method"]["native_resources_and_verifier_timeouts"] = True
    manifest["method"]["guest_async_progress_changed_by_authorized_pause"] = True
    manifest["budget"]["per_task_agent_timeout"] = "native except explicit agent_timeout_overrides"
    manifest["budget"]["agent_timeout_overrides"] = agent_timeouts
    atomic_json(manifest_path, manifest)
    atomic_json(existing, {
        "schema": SCHEMA, "status": "prepared", "updated_at": utc(),
        "hostname": socket.getfqdn(), "shard_index": args.shard_index,
        "shard_count": args.shard_count, "total_tasks": len(selected),
        "paid_attempts": 0, "task_counts": {"pending": len(selected)}, "active_task": None,
    })
    print(json.dumps({"status": "prepared", "manifest": str(manifest_path),
                      "catalog_tasks": len(records), "shard_tasks": len(selected),
                      "profiles": [r["profile"] for r in selected]}))



def chown_tree(path, owner):
    if os.geteuid() != 0:
        return
    account = pwd.getpwnam(owner)
    for directory, dirs, files in os.walk(path, followlinks=False):
        os.chown(directory, account.pw_uid, account.pw_gid, follow_symlinks=False)
        for name in dirs + files:
            os.chown(Path(directory) / name, account.pw_uid, account.pw_gid, follow_symlinks=False)


def read_json(path):
    return json.loads(Path(path).read_text())


def read_vmm_config(path):
    if path is None:
        raise ValueError("VMM-owned observer config required")
    config = read_json(inside_repo(path))
    if not isinstance(config, dict) or config.get("backend") != "vmm_owned":
        raise ValueError("observer requires backend=vmm_owned")
    return config


def prepared_vmm_config(manifest):
    binding = manifest["execution"].get("access_dirty_config")
    if not isinstance(binding, dict) or not binding.get("path"):
        raise ValueError("VMM-owned observer config required")
    config = read_vmm_config(binding["path"])
    if digest(binding["path"]) != binding.get("sha256"):
        raise ValueError("access-dirty config changed after preparation")
    if manifest["execution"].get("observation_mode") != "group":
        raise ValueError("observer requires group observation mode")
    return config


def read_rows(path):
    return [json.loads(line) for line in Path(path).read_text().splitlines() if line.strip()]


def process_rows():
    """Read identities only; never persist process arguments or environments."""
    rows = []
    for directory in Path("/proc").iterdir():
        if not directory.name.isdigit():
            continue
        try:
            args = (directory / "cmdline").read_bytes().split(b"\0")
            executable = Path(os.fsdecode(args[0])).name
            record = {"pid": int(directory.name)}
            if "firecracker" in executable and b"--id" in args:
                at = args.index(b"--id")
                vm_id = os.fsdecode(args[at + 1])
                if not vm_id.startswith("aries-"):
                    continue
                record.update(kind="firecracker", vm_id=vm_id)
            else:
                continue
            rows.append(record)
        except (OSError, ValueError, IndexError):
            continue
    return rows


def inventory():
    """A dedicated worker must be empty between tasks; never remove foreign state."""
    sock = "/run/firecracker-containerd/containerd.sock"
    commands = {
        "docker_containers": ["docker", "ps", "-a", "--filter", "label=aries.managed=true",
                              "--format", "{{json .}}"],
        "docker_networks": ["docker", "network", "ls", "--filter", "label=aries.managed=true",
                            "--format", "{{json .}}"],
        "containerd_containers": ["ctr", "--address", sock, "--namespace", "aries",
                                  "containers", "list", "-q"],
        "containerd_tasks": ["ctr", "--address", sock, "--namespace", "aries",
                             "tasks", "list", "-q"],
    }
    result = {"at": utc(), "errors": []}
    for name, command in commands.items():
        try:
            output = subprocess.check_output(command, text=True, stderr=subprocess.PIPE, timeout=30)
            if name.startswith("docker_"):
                # Only ownership identifiers are retained, never container env/config.
                records = [json.loads(line) for line in output.splitlines() if line.strip()]
                result[name] = [{key: row.get(key) for key in ("ID", "Name", "Names", "Labels")
                                 if key in row} for row in records]
            else:
                result[name] = output.split()
        except (subprocess.SubprocessError, OSError, ValueError) as error:
            result[name] = None
            result["errors"].append(name + ": " + type(error).__name__)
    processes = process_rows()
    result["firecracker_processes"] = [r for r in processes if r["kind"] == "firecracker"]
    result["clean"] = not result["errors"] and not any(result.get(name) for name in (
        "docker_containers", "docker_networks", "containerd_containers", "containerd_tasks",
        "firecracker_processes"))
    return result



def canonical_memory_regions(regions):
    result = []
    for r in regions:
        base, size, slot = r["guest_base"], r["bytes"], r["slot"]
        if (type(base) is not int or type(size) is not int or base < 0
                or size <= 0 or base + size > 1 << 64 or base % 4096 or size % 4096
                or type(slot) is not int or not 0 <= slot < 1 << 32
                or type(r["page_count"]) is not int or r["page_count"] != size // 4096):
            raise ValueError("invalid VMM RAM region")
        result.append((base, size, slot))
    result.sort()
    if not result or len({r[2] for r in result}) != len(result):
        raise ValueError("missing/duplicate VMM RAM slots")
    if any(a[0] + a[1] > b[0] for a, b in zip(result, result[1:])):
        raise ValueError("overlapping VMM RAM regions")
    return result


def observation_memory(observer, readings):
    """Validate retained authoritative RAM identity, without live process reads."""
    initial = read_json(observer / "access-dirty/initial-arm.json")
    generation = initial.get("vm_generation")
    if (initial.get("schema") != "aries.firecracker.trace-epochs.v1"
            or not isinstance(generation, str) or not generation
            or initial.get("baseline_discarded") is not True):
        raise ValueError("missing or invalid VMM initial-arm identity")
    expected = canonical_memory_regions(initial["regions"])
    for group in readings:
        reading = group["reading"]
        if (reading.get("vm_generation") != generation
                or canonical_memory_regions(reading["regions"]) != expected):
            raise ValueError("reading differs from authoritative VMM generation/regions")
    return sum(r[1] for r in expected), "vmm_initial_arm_authoritative_regions"

class Suite:
    def __init__(self, args, manifest):
        self.args = args
        self.root = Path(manifest["root"])
        self.manifest = manifest
        self.state_path = self.root / "state" / ("shard-" + str(manifest["shard_index"]) + ".json")
        self.events_path = self.root / ("events-shard-" + str(manifest["shard_index"]) + ".jsonl")
        self.tasks = selected_records(manifest)
        self.stop_requested = False
        self.halt_reasons = []
        self.active_task = None
        self.started_at = utc()
        self.phase = "running"
        self.last_disk_check = 0
        self.last_heartbeat = 0

    def event(self, event, **fields):
        entry = {"at": utc(), "event": event, "task_id": self.active_task, **fields}
        with self.events_path.open("a") as stream:
            stream.write(json.dumps(entry) + "\n")
            stream.flush()
            os.fsync(stream.fileno())
            set_owner(stream.fileno())
        print(json.dumps(entry), flush=True)

    def halt(self, reason):
        if reason not in self.halt_reasons:
            self.halt_reasons.append(reason)
            self.event("admission_stopped", reason=reason)
        self.stop_requested = True

    def handle_signal(self, signum, _frame):
        # Avoid filesystem/stdio work in the signal handler; the next state
        # update persists the request while native cleanup keeps running.
        reason = ("received signal " + signal.Signals(signum).name +
                  "; waiting for native task lifecycle without forced termination")
        if reason not in self.halt_reasons:
            self.halt_reasons.append(reason)
        self.stop_requested = True

    def disk_ok(self, force=False):
        if not force and time.monotonic() - self.last_disk_check < 15:
            return not any(r.startswith("disk reserve") for r in self.halt_reasons)
        self.last_disk_check = time.monotonic()
        available = shutil.disk_usage(self.root).free
        if available < self.manifest["execution"]["min_free_gib"] * 1024 ** 3:
            self.halt("disk reserve crossed: free_bytes=" + str(available))
            return False
        return True

    def save_task(self, task_status, **fields):
        task_status.update(fields)
        task_status["updated_at"] = utc()
        atomic_json(self.root / task_status["task_id"] / "status.json", task_status)
        self.save_state()

    def save_state(self):
        records = [read_json(self.root / task["task_id"] / "status.json") for task in self.tasks]
        atomic_json(self.state_path, {
            "schema": SCHEMA, "status": self.phase, "updated_at": utc(),
            "started_at": self.started_at, "hostname": socket.getfqdn(),
            "shard_index": self.manifest["shard_index"], "shard_count": self.manifest["shard_count"],
            "total_tasks": len(records), "active_task": self.active_task,
            "paid_attempts": sum(r["paid_attempts"] for r in records),
            "task_counts": dict(collections.Counter(r["status"] for r in records)),
            "harness_counts": dict(collections.Counter(r.get("harness", {}).get("status", "not_started")
                                                       for r in records)),
            "evaluation_counts": dict(collections.Counter(r.get("evaluation", {}).get("status", "not_run")
                                                          for r in records)),
            "measurement_counts": dict(collections.Counter(r.get("measurement", {}).get("status", "not_observed")
                                                           for r in records)),
            "boundary_audit_counts": dict(collections.Counter(r.get("boundary_audit", {}).get("status", "not_run")
                                                              for r in records)),
            "stop_requested": self.stop_requested, "halt_reasons": self.halt_reasons,
            "disk_free_bytes": shutil.disk_usage(self.root).free,
        })

    def wait(self, process, status):
        while process.poll() is None:
            self.disk_ok()
            self.recover_fatal_reader(status)
            self.refresh_capture(status)
            if time.monotonic() - self.last_heartbeat >= 30:
                self.last_heartbeat = time.monotonic()
                self.refresh_capture(status)
                self.save_task(status, heartbeat_at=utc())
            time.sleep(1)
        self.refresh_capture(status)
        return process.returncode

    def recover_fatal_reader(self, status):
        process = getattr(self, "observer_process", None)
        if process is None or status.get("fatal_reader_recovery") or process.poll() is None:
            return
        receipt_path = self.root / status["task_id"] / "observer/access-dirty/fatal-reader.json"
        if not receipt_path.exists():
            return
        receipt = read_json(receipt_path)
        if receipt["observer_pid"] != process.pid or process.wait(timeout=1) != 86:
            raise RuntimeError("fatal observer identity/exit mismatch")
        # poll/wait reaps this owned child; all metadata threads are now gone.
        pid = receipt["vmm_pid"]
        def identity():
            stat = Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()
            argv = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
            vm_id = os.fsdecode(argv[argv.index(b"--id") + 1])
            if int(stat[19]) != int(receipt["vmm_start_ticks"]) or vm_id != receipt["vm_id"] or not vm_id.startswith(status["vm_prefix"]):
                raise RuntimeError("fatal recovery VMM identity mismatch")
        identity()
        from vmm_observer import Connection
        conn = Connection(receipt["socket_path"], 10)
        try:
            conn.request("PATCH", "/vm", json.dumps({"state": "Resumed"}), {"Content-Type": "application/json"})
            response = conn.getresponse(); body = response.read()
            if response.status != 204:
                raise RuntimeError("fatal recovery Resume HTTP " + str(response.status))
        finally:
            conn.close()
        identity()
        status["fatal_reader_recovery"] = {"observer_reaped": True, "vmm_identity_confirmed": True,
                                         "resumed_at": utc(), "measurement_valid": False}
        status.setdefault("measurement_errors", []).append("fatal metadata reader; observer exited before VM release")
        self.halt("fatal observer reader: measurement invalid; native lifecycle cleanup retained")
        self.save_task(status)

    def refresh_capture(self, status):
        if "capture" in status:
            return
        observer = self.root / status["task_id"] / "observer"
        try:
            identity = read_json(observer / "vmm.json")
            pid = identity["pid"]
            args = Path("/proc/" + str(pid) + "/cmdline").read_bytes().split(b"\0")
            vm_id = os.fsdecode(args[args.index(b"--id") + 1])
            start = int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])
            if (start != int(identity["start_ticks"]) or vm_id != identity["vm_id"]
                    or not vm_id.startswith(status["vm_prefix"])):
                raise ValueError("selected VMM ID does not match task-specific prefix")
            capture = {
                "vmm_pid": pid, "vmm_start_ticks": identity["start_ticks"], "vm_id": vm_id,
                "vmm_json_sha256": digest(observer / "vmm.json"),
            }
            memory, evidence = observation_memory(observer, [])
            initial_path = observer / "access-dirty/initial-arm.json"
            initial = read_json(initial_path)
            capture.update(vm_generation=initial["vm_generation"],
                selected_slots=len(initial["regions"]), selected_slot_memory_bytes=memory,
                memory_region_evidence=evidence, initial_arm_sha256=digest(initial_path))
            status["capture"] = capture
            self.event("vmm_selected", **status["capture"])
        except (OSError, KeyError, ValueError, IndexError, TypeError):
            # Files can be between native write calls. Final evidence parsing
            # records any unresolved selection failure after the observer stops.
            return

    def command(self, command, name, status):
        task_root = self.root / status["task_id"]
        with open_log(task_root / (name + ".stdout.log")) as stdout, \
                open_log(task_root / (name + ".stderr.log")) as stderr:
            process = subprocess.Popen(command, cwd=REPO, stdout=stdout, stderr=stderr,
                                       start_new_session=True)
            self.save_task(status, **{name + "_pid": process.pid})
            self.event("command_started", command=name, pid=process.pid)
            result = self.wait(process, status)
            self.event("command_finished", command=name, returncode=result)
            return result

    def observer_ready(self, process, task_root):
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline and process.poll() is None:
            try:
                ready = read_json(task_root / "observer/ready.json")
                if ready["pid"] != process.pid:
                    raise RuntimeError("observer readiness identity differs")
                with urllib.request.urlopen("http://127.0.0.1:" + str(self.manifest["execution"]["observer_port"]),
                                            timeout=2) as response:
                    health = json.load(response)
                if health.get("ready") is True and health.get("invalid") is False:
                    return
            except (OSError, ValueError, KeyError, urllib.error.URLError):
                pass
            time.sleep(.2)
        raise RuntimeError("observer failed readiness before the native run")

    def stop_observer(self, process, status):
        if process.poll() is None:
            process.send_signal(signal.SIGTERM)
        try:
            code = process.wait(timeout=30)
            status["observer_returncode"] = code
            self.recover_fatal_reader(status)
            if code:
                status.setdefault("measurement_errors", []).append("observer exited " + str(code))
        except subprocess.TimeoutExpired:
            # Leave ownership/evidence intact for inspection; do not kill a
            # process that may still be finalizing an observation.
            status["observer_stop_unconfirmed"] = True
            self.halt("observer did not stop within its native finalization allowance")

    def analyze(self, record, status):
        task_root = self.root / record["task_id"]
        observer = task_root / "observer"
        try:
            rows = read_rows(observer / "steps.jsonl")
            errors = read_rows(observer / "errors.jsonl")
            begins = [r for r in rows if r.get("event") == "step_begin"]
            ends = [r for r in rows if r.get("event") == "step_end"]
            mode = "group"
            groups_path = observer / "groups.jsonl"
            readings = [r for r in read_rows(groups_path) if r.get("event") == "group_end"]
            grouped_members = collections.Counter((group.get("batch_id"), call)
                for group in readings for call in group.get("member_tool_call_ids", []))
            result_ids = collections.Counter((row.get("batch_id"), row["tool_call_id"]) for row in ends)
            group_pairing = grouped_members == result_ids
            admission_ids = collections.Counter((row.get("batch_id"), row["tool_call_id"]) for row in begins)
            admitted_results = collections.Counter((row.get("batch_id"), row["tool_call_id"])
                for row in ends if row.get("begin_observed") is True)
            admission_pairing = admission_ids == admitted_results
            completed_window_evidence_valid = (
                admission_pairing and group_pairing
                and all(r.get("measurement_valid") is True for r in ends + readings)
                and status.get("observer_returncode") == 0)
            vm_ids = sorted({r["reading"]["vm_instance"] for r in readings})
            slot_bytes, region_source = observation_memory(observer, readings)
            native_memory = record["native_environment"]["memory_mb"] * 1024 ** 2
            if readings and (len(vm_ids) != 1 or not vm_ids[0].startswith(record["vm_prefix"])
                         or slot_bytes != native_memory):
                completed_window_evidence_valid = False
            valid = completed_window_evidence_valid and not errors
            status["measurement"] = {
                "status": ("succeeded" if valid else "failed") if rows or errors else "not_observed",
                "completed_window_evidence_valid": completed_window_evidence_valid,
                "step_begins": len(begins), "step_ends": len(ends),
                "mode": mode, "comparison_unit": "llm_reasoning_step",
                "reading_count": len(readings), "llm_steps": len(readings),
                "native_results_without_admission": sum(row.get("begin_observed") is False for row in ends),
                "coverage_complete": False,
                "measurement_valid_scope": "expected_batch_registration_and_result_completion_only",
                "groups_sha256": digest(groups_path) if groups_path.exists() else None,
                "interior_reset_gaps": sum(len(r["reading"].get("transition_gaps", [])) for r in readings),
                "overlap_readings": sum(bool(r["reading"].get("overlapping_tool_call_ids")) for r in readings),
                "observer_error_count": len(errors), "invalid_steps": sum(
                    r.get("measurement_valid") is not True for r in ends + readings),
                "vm_ids": vm_ids, "selected_slot_memory_bytes": slot_bytes,
                "native_memory_bytes": native_memory,
                "memory_region_evidence": region_source,
                "native_memory_matches_selected_slots": slot_bytes == native_memory if slot_bytes is not None else None,
                "steps_sha256": digest(observer / "steps.jsonl"),
                "errors_sha256": digest(observer / "errors.jsonl"),
                "unknown_policy": "raw unknown classifications retained; compare counts unknown as touched by requested assumption",
            }
            code = self.command([sys.executable, "-B", str(REPO / "scripts/tsb-pages/compare.py"),
                                 "--input", str(groups_path),
                                 "--mode", mode,
                                 "--output", str(task_root / "comparison.json")], "compare", status)
            status["comparison"] = {"status": "succeeded" if code == 0 else "failed", "returncode": code}
        except (OSError, ValueError, KeyError, TypeError) as error:
            status["measurement"] = {"status": "failed", "error_type": type(error).__name__,
                                      "error": str(error)}
        runs = sorted((task_root / "runs").glob("*/run-result.json"))
        # Include failed/partial observations, even if the native run result is absent.
        overhead_command = [sys.executable, "-B", str(REPO / "scripts/tsb-pages/overhead.py"),
                            "--observer", str(observer), "--output", str(task_root / "overhead.json")]
        if len(runs) == 1:
            plugin_log = runs[0].parent / record["execution_task_id"] / "harness/telemetry/aries-step-pages.jsonl"
            if plugin_log.exists():
                overhead_command.extend(["--plugin-log", str(plugin_log)])
        try:
            code = self.command(overhead_command, "overhead", status)
            overhead_report = read_json(task_root / "overhead.json") if code == 0 else {}
            status["tracer_overhead"] = {"status": overhead_report.get("status", "failed"),
                                        "returncode": code, "errors": overhead_report.get("errors", [])}
        except (OSError, ValueError, KeyError, TypeError) as error:
            status["tracer_overhead"] = {"status": "failed", "error_type": type(error).__name__,
                                        "error": str(error)}
        if len(runs) != 1:
            status["result_error"] = "expected exactly one run-result.json; found " + str(len(runs))
            self.halt("native result missing or ambiguous; cleanup outcome cannot be established")
            return
        result_path = runs[0]
        result = read_json(result_path)
        status["run_root"] = str(result_path.parent)
        status["run_id"] = result.get("run_id")
        status["run_result_sha256"] = digest(result_path)
        status["run_summary"] = result.get("summary")
        task_results = result.get("tasks", [])
        if len(task_results) != 1 or task_results[0].get("task_id") != record["execution_task_id"]:
            self.halt("native result task identity differs from the one-task profile")
        elif task_results:
            task = task_results[0]
            for field in ("harness", "evaluation", "isolation", "cleanup", "observer"):
                value = dict(task.get(field, {}))
                value.pop("final_response", None)
                status[field] = value
            status["task_error"] = task.get("error")
            if task.get("cleanup", {}).get("status") not in ("succeeded", "not_needed"):
                self.halt("native task cleanup failed or is unconfirmed")
        infrastructure = result.get("infrastructure") or {}
        status["infrastructure"] = infrastructure
        if infrastructure and infrastructure.get("cleanup", {}).get("status") not in ("succeeded", "not_needed"):
            self.halt("native shared infrastructure cleanup failed or is unconfirmed")
        telemetry = result_path.parent / record["execution_task_id"] / "harness/telemetry/aries-step-pages.jsonl"
        if telemetry.exists():
            try:
                code = self.command([sys.executable, "-B", str(REPO / "scripts/tsb-pages/boundaries.py"),
                                     "--run", str(result_path.parent), "--observer-run", str(task_root),
                                     "--task-id", record["execution_task_id"]], "boundaries", status)
                audit_path = task_root / "boundary-audit.json"
                audit = read_json(audit_path) if audit_path.exists() else {}
                status["boundary_audit"] = {"status": audit.get("status", "failed"),
                                            "returncode": code, "steps": len(audit.get("steps", [])),
                                            "failed_checks": [c for c in audit.get("checks", []) if not c["passed"]]}
                if code == 0:
                    reconcile_terminal_censor(status.get("measurement", {}),
                                              status.get("comparison", {}), audit, digest(audit_path))
            except (OSError, ValueError, KeyError, TypeError) as error:
                status["boundary_audit"] = {"status": "failed", "error_type": type(error).__name__,
                                            "error": str(error)}
        else:
            status["boundary_audit"] = {"status": "not_run", "reason": "native hook telemetry absent"}

    def task(self, record):
        task_root = self.root / record["task_id"]
        status = read_json(task_root / "status.json")
        observer = None
        observer_logs = []
        self.active_task = record["task_id"]
        status["vm_prefix"] = record["vm_prefix"]
        self.save_task(status, status="setup", started_at=utc())
        try:
            validate_observer_override(record)
            setup_code = self.command([str(REPO / "bin/aries"), "setup", record["profile"]], "setup", status)
            self.save_task(status, setup_returncode=setup_code)
            if setup_code != 0:
                self.save_task(status, status="setup_failed", finished_at=utc())
                return
            after_setup = inventory()
            atomic_json(task_root / "after-setup-inventory.json", after_setup)
            if not after_setup["clean"]:
                self.halt("runtime residue or inspection failure after setup")
                self.save_task(status, status="cleanup_failed", finished_at=utc())
                return
            if not self.disk_ok(force=True) or self.stop_requested:
                self.save_task(status, status="not_launched", finished_at=utc())
                return
            for suffix in ("stdout", "stderr"):
                observer_logs.append(open_log(task_root / ("observer." + suffix + ".log")))
            self.save_task(status, status="observer_starting")
            observer_command = [
                sys.executable, "-B", str(REPO / "scripts/tsb-pages/serve.py"),
                "--output", str(task_root / "observer"),
                "--port", str(self.manifest["execution"]["observer_port"]),
                "--vm-prefix", record["vm_prefix"],
            ]
            access_dirty = self.manifest["execution"]["access_dirty_config"]
            selected_config = prepared_vmm_config(self.manifest)
            for source_path, expected_hash in selected_config.get("companion_source_hashes", {}).items():
                if digest(source_path) != expected_hash:
                    raise ValueError("VMM companion changed after preparation")
            observer_config = validate_observer_override(record) or access_dirty["path"]
            observer_command.extend(["--access-dirty-config", observer_config])
            attribution = self.manifest["execution"].get("attribution_config")
            if attribution and record.get("detailed_attribution"):
                if digest(attribution["path"]) != attribution["sha256"]:
                    raise ValueError("attribution config changed after preparation")
                for key, source in attribution.get("inputs", {}).items():
                    if digest(source["path"]) != source["sha256"]:
                        raise ValueError("attribution input changed after preparation: " + key)
                observer_command.extend(["--attribution-config", attribution["path"]])
            observer = subprocess.Popen(observer_command, cwd=REPO, stdout=observer_logs[0],
                                        stderr=observer_logs[1], start_new_session=True)
            self.observer_process = observer
            self.save_task(status, observer_pid=observer.pid)
            self.observer_ready(observer, task_root)
            if self.stop_requested:
                self.save_task(status, status="not_launched", finished_at=utc())
                return
            # Reserve this sole attempt durably before process creation, so
            # interruption cannot cause an ambiguous native submission retry.
            self.save_task(status, status="running", paid_attempts=1, native_started_at=utc())
            self.event("native_attempt_reserved", paid_attempts=1)
            code = self.command([str(REPO / "bin/aries"), record["profile"]], "aries", status)
            self.save_task(status, aries_returncode=code, native_finished_at=utc())
        except Exception as error:
            status["driver_error"] = {"type": type(error).__name__, "error": str(error)}
            self.halt("driver exception; inspect retained task artifacts before any further admission")
        finally:
            if observer is not None:
                try:
                    self.stop_observer(observer, status)
                except Exception as error:
                    status["observer_stop_error"] = {"type": type(error).__name__, "error": str(error)}
                    self.halt("observer stop failed")
            for stream in observer_logs:
                stream.close()
            try:
                if status["paid_attempts"]:
                    self.analyze(record, status)
                    if status.get("measurement", {}).get("status") not in ("succeeded", "censored"):
                        self.halt("measurement failed; retain evidence and inspect before further admission")
                    if status.get("boundary_audit", {}).get("status") not in ("passed", "censored"):
                        self.halt("boundary audit failed; inspect before further admission")
            except Exception as error:
                status["analysis_error"] = {"type": type(error).__name__, "error": str(error)}
                self.halt("native result analysis failed; cleanup outcome requires inspection")
            try:
                after = inventory()
                atomic_json(task_root / "after-task-inventory.json", after)
                status["runtime_absence_confirmed"] = after["clean"]
                if not after["clean"]:
                    self.halt("owned runtime residue or inspection failure after native cleanup")
            except Exception as error:
                status["runtime_absence_confirmed"] = False
                status["inventory_error_type"] = type(error).__name__
                self.halt("post-task runtime inventory failed")
            if status["paid_attempts"]:
                status["status"] = "completed" if status.get("aries_returncode") == 0 else "failed"
            elif status["status"] not in ("setup_failed", "not_launched", "cleanup_failed"):
                status["status"] = "failed"
            if not status.get("runtime_absence_confirmed") or status.get("observer_stop_unconfirmed"):
                status["status"] = "cleanup_failed"
            self.save_task(status, finished_at=utc())
            try:
                chown_tree(task_root, self.manifest["execution"]["artifact_owner"])
            except OSError as error:
                status["artifact_owner_error"] = type(error).__name__
                self.halt("could not return artifact ownership to the research account")
                self.save_task(status)
            self.event("task_finished", status=status["status"],
                       harness=status.get("harness", {}).get("status"),
                       evaluation=status.get("evaluation", {}).get("status"),
                       score=status.get("evaluation", {}).get("score"),
                       measurement=status.get("measurement", {}).get("status"))
            self.active_task = None
            self.save_state()

    def execute(self):
        original = {}
        for signum in (signal.SIGINT, signal.SIGTERM):
            original[signum] = signal.signal(signum, self.handle_signal)
        try:
            self.save_state()
            self.event("suite_started", shard_index=self.manifest["shard_index"],
                       tasks=len(self.tasks), automatic_paid_retries=0)
            before = inventory()
            atomic_json(self.root / ("before-suite-inventory-shard-" +
                                      str(self.manifest["shard_index"]) + ".json"), before)
            if not before["clean"]:
                self.halt("worker has pre-existing ARIES runtimes or inspection failures")
            for record in self.tasks:
                if self.stop_requested or not self.disk_ok(force=True):
                    break
                self.task(record)
            self.phase = "halted" if self.stop_requested else "finished"
            self.save_state()
            self.event("suite_finished", status=self.phase)
            return 1 if self.stop_requested else 0
        finally:
            self.active_task = None
            self.save_state()
            for signum, handler in original.items():
                signal.signal(signum, handler)


def set_owner(fd):
    if os.geteuid() == 0 and ARTIFACT_OWNER is not None:
        os.fchown(fd, ARTIFACT_OWNER.pw_uid, ARTIFACT_OWNER.pw_gid)


def open_log(path):
    stream = Path(path).open("xb")
    set_owner(stream.fileno())
    return stream


def run_suite(args):
    if os.geteuid() != 0:
        raise ValueError("run requires root for page metadata and the existing native key-file fallback")
    root = inside_repo(args.root)
    manifest_path = root / "manifests" / ("shard-" + str(args.shard_index) + ".json")
    state_path = root / "state" / ("shard-" + str(args.shard_index) + ".json")
    manifest = read_json(manifest_path)
    prepared_vmm_config(manifest)
    state = read_json(state_path)
    if state["status"] != "prepared":
        raise ValueError("suite has already started; automatic native/paid retries and restarts are disabled")
    if manifest["root"] != str(root) or manifest["hostname"] != socket.getfqdn():
        raise ValueError("manifest belongs to a different root or host; prepare independently on this worker")
    if manifest["shard_count"] != args.shard_count:
        raise ValueError("run shard count must match prepared assignment")
    if manifest["model"] != json.loads(TEMPLATE.read_text())["model"]:
        raise ValueError("model changed since prepare")
    if manifest["source"] != source_record():
        raise ValueError("source/binary/observer changed since prepare; regenerate pending profiles offline")
    _, catalog = task_catalog(args.shard_count)
    expected = [{k: v for k, v in record.items() if k not in ("profile", "profile_sha256", "detailed_attribution", "runtime_overrides", "observer_override")}
                for record in manifest["tasks"]]
    if catalog != expected:
        raise ValueError("native task catalog changed since prepare")
    for record in selected_records(manifest):
        status = read_json(root / record["task_id"] / "status.json")
        if status["status"] != "pending" or status["paid_attempts"] != 0:
            raise ValueError("task already entered lifecycle; refusing a retry")
        if record.get("runtime_overrides") and digest(record["runtime_overrides"]["path"]) != record["runtime_overrides"]["sha256"]:
            raise ValueError("runtime timeout override changed since prepare")
        if digest(record["profile"]) != record["profile_sha256"]:
            raise ValueError("profile changed since prepare")
        validate_observer_override(record)
    suite = Suite(args, manifest)
    return suite.execute()



def main():
    parser = argparse.ArgumentParser(description=__doc__)
    actions = parser.add_mutually_exclusive_group(required=True)
    actions.add_argument("--prepare", action="store_true", help="offline manifest/profile generation only")
    actions.add_argument("--run", action="store_true", help="one native paid attempt per selected task")
    parser.add_argument("--root", type=Path, default=DEFAULT_ROOT)
    parser.add_argument("--selection-file", type=Path,
                        help="prepare only: explicit JSON task_ids and reason for a fresh root")
    parser.add_argument("--shard-index", type=int, default=0)
    parser.add_argument("--shard-count", type=int, default=1)
    parser.add_argument("--port", type=int, default=18181)
    parser.add_argument("--min-free-gib", type=float, default=20)
    parser.add_argument("--owner", default="nehalem")
    parser.add_argument("--observation-mode", choices=("group",), default="group",
                        help="one LLM reasoning-step batch; preserves native scheduling")
    parser.add_argument("--attribution-config", type=Path,
                        help="prepare only: optional VMM companion attribution config")
    parser.add_argument("--access-dirty-config", type=Path,
                        help="required for prepare: VMM-owned config with backend=vmm_owned")
    args = parser.parse_args()
    if args.access_dirty_config is not None and not args.prepare:
        parser.error("--access-dirty-config applies only to --prepare")
    if args.attribution_config is not None and not args.prepare:
        parser.error("--attribution-config applies only to --prepare; run uses its frozen manifest")
    if args.selection_file is not None and not args.prepare:
        parser.error("--selection-file applies only to --prepare; run uses its frozen manifest")
    if not 0 <= args.shard_index < args.shard_count <= EXPECTED_TASKS:
        parser.error("require 0 <= shard-index < shard-count <= 89")
    if not 1024 <= args.port <= 65535 or args.min_free_gib < 1:
        parser.error("require an unprivileged valid port and positive disk reserve")
    root = inside_repo(args.root)
    if args.prepare:
        read_vmm_config(args.access_dirty_config)
    else:
        prepared_vmm_config(read_json(root / "manifests" / ("shard-" + str(args.shard_index) + ".json")))
    global ARTIFACT_OWNER
    ARTIFACT_OWNER = pwd.getpwnam(args.owner)
    os.chdir(REPO)
    os.umask(0o077)
    root.mkdir(parents=True, exist_ok=True)
    with (root / ".suite.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if args.prepare:
            prepare(args)
            chown_tree(root, args.owner)
        else:
            return run_suite(args)


if __name__ == "__main__":
    try:
        sys.exit(main() or 0)
    except Exception as error:
        print(json.dumps({"status": "error", "error_type": type(error).__name__,
                          "error": str(error)}), file=sys.stderr)
        sys.exit(1)
