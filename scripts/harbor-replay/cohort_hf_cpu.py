"""Source checks for the original HF app that selects two Torch threads."""
from ast import literal_eval
from math import isclose

from cohort_pypi import pinned

TRIAL = "071ce48d-3de4-4c02-9e42-3eb64933acd2"
HOST_CPU = 15
ROOT_SHA = "292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f"
PLUGINS_COMMIT = "1e285826e604f66f7208f7ac4dba0fe8341d1f57"
PLUGINS_SHA = "6482d01621431880a077fb4305b245bc6a7909a8e8f875cc4ea06c7c6f268d4e"
MODEL_DIGESTS = {
    "README.md":"75f17282923fea5b541df92640e2d8646c4ff5e8e3e16ed0a7e4e50ee7588851",
    "config.json":"582122c8f414793d131e10022ce9ba04e3811a9da6389137ee2f18665b4f4d15",
    "model.safetensors":"7c3919835e442510166d267fe7cbe847e0c51cd26d9ba07b89a57b952b49b8aa",
    "tokenizer_config.json":"5ab9097b4149371c5fd52b2d6e26cb6f9c07c0d19fcfdda895b1adad6b57c3e0",
    "vocab.txt":"07eced375cec144d27c900241f3e339478dec958f92fddbc551f295c992038a3",
}
SOURCE_SCORES = {"negative":0.00012293607869651169, "positive":0.9998770952224731}
TOOLS_SHA = "36ae1b315f30cab6e9b06c4e81d7cb8e512658a2b799108b9080ff1b6999a74c"
INSTRUCTION_PATHS = {
    "/tmp/codex-home/.tmp/plugins/plugins/" + path + "/AGENTS.md"
    for path in ("build-web-apps/skills/react-best-practices", "build-web-apps/skills/supabase-best-practices",
                 "data-analytics", "product-design/templates/mobile-app", "product-design/templates/prototype",
                 "vercel/skills/react-best-practices", "zoom")
}


def replay_args(asset):
    pinned(asset/"upstream.tar.gz", PLUGINS_SHA)
    pinned(asset/"hf-tools.tar.gz", TOOLS_SHA)
    return ["-hf-harness", str(asset), "-hf-host-cpu", str(HOST_CPU)]


def evidence(events, ready):
    first = events[0]
    expected = {"host_cpu": HOST_CPU, "guest_vcpus": 2, "requested_physical_cpus": 1, "source_qualified": True}
    if first.get("hf_cpu_profile") != expected or first["vcpus"] != 2 or first["source_requested_cpus"] != 1:
        raise ValueError("original physical CPU request or guest logical count differs")
    if (ready.get("inspection_tool_archive_sha256") != TOOLS_SHA or
            ready.get("curated_plugins_archive_sha256") != PLUGINS_SHA or ready.get("curated_plugins_commit") != PLUGINS_COMMIT):
        raise ValueError("dated inference inspection fixture differs")
    resources = [e for e in events if e["event"] == "source_hf_cpu_readback"]
    if [e["phase"] for e in resources] != ["before_snapshot", "after_restore", "after_source"]:
        raise ValueError("missing observed guest CPU or host thread checks")
    for row in resources:
        host, guest = row["host"], row["guest"]
        threads = host["vcpu_threads"]
        if (host["host_cpu"] != HOST_CPU or host["cpuset_effective"] != str(HOST_CPU) or
                host["host_smt_active"] != "0" or host["thread_siblings"] != str(HOST_CPU) or
                {x["name"] for x in threads} != {"fc_vcpu 0", "fc_vcpu 1"} or
                any(t["allowed"] != [HOST_CPU] or t["last_cpu"] != HOST_CPU for t in threads) or
                guest["guest_logical_cpus"] != 2 or guest["guest_allowed_cpus"] != [0,1]):
            raise ValueError("live guest CPUs and their single host-core allocation differ")
    guest = resources[-1]["guest"]
    if guest.get("model_file_sha256") != MODEL_DIGESTS:
        raise ValueError("the real downloaded model files differ from the pinned revision")
    server, = guest["servers"]
    if (guest["probe_torch_threads"] != 2 or guest["original_thread_selection"] != "torch.set_num_threads(min(2, os.cpu_count() or 1))" or
            not server["no_python_options"] or server["cpu_count_override"] or server["affinity"] != [0,1] or
            server["serial_predictions"] != ["positive"] * 4 or sum(server["serial_prediction_thread_cpu_ns"].values()) <= 0):
        raise ValueError("original service thread configuration or live inference differs")
    calls = [e for e in events if e["event"] == "tool_reply"]
    text = lambda e: e.get("stdout", "") + "\n" + e.get("stderr", "")
    original_cpu, = [e for e in calls if e["step_id"] == 8]
    scan, = [e for e in calls if e["step_id"] == 10 and "/usr/bin/patch" in text(e).splitlines()]
    scan_poll, = [e for e in calls if e["step_id"] == 12]
    if ("CPU count: 2" not in text(original_cpu).splitlines() or
            not {"/usr/bin/git","/usr/bin/patch","/usr/bin/curl"} <= set(text(scan).splitlines()) or
            not INSTRUCTION_PATHS <= set(text(scan_poll).splitlines()) or
            "All 32 integration checks passed." not in text(calls[-1]).splitlines()):
        raise ValueError("original HF CPU, inspection files or full integration results differ")
    offline, = [e for e in calls if e["step_id"] == 13]
    scores, = [literal_eval(line) for line in text(offline).splitlines() if line.startswith("{'negative':")]
    if scores.keys() != SOURCE_SCORES.keys() or not all(isclose(scores[k],v,rel_tol=0,abs_tol=1e-8) for k,v in SOURCE_SCORES.items()):
        raise ValueError("original offline sentiment probabilities differ")
    return {"model_file_sha256":MODEL_DIGESTS, "source_probabilities":SOURCE_SCORES, "guest_probabilities":scores,
            "requested_physical_cpus": 1, "visible_vcpus": 2, "host_cpu": HOST_CPU,
            "server_torch_thread_selection": 2, "verified_source_instructions": sorted(INSTRUCTION_PATHS),
            "original_integration_checks": 32, "post_source_serial_predictions": 4}
