"""Summarize and audit the successful full DeltaBox calendar-arm receipts."""
import argparse
import json
import statistics
from pathlib import Path


def summarize(path):
    rows = [json.loads(line) for line in path.read_text().splitlines()]
    verified, = [r for r in rows if r["event"] == "verified"]
    assert rows[-1]["event"] == "qemu_stopped"
    assert not any(r["event"] == "first_failure" for r in rows)
    host = [r for r in rows if r["event"] == "host_sample"]
    assert len({r["pid"] for r in host}) == 1
    phases = {r["phase"]: r for r in host if r["phase"] != "periodic"}
    start = verified["hold_start_ns"]
    stop = verified["hold_end_ns"]
    hold = [r for r in host if start <= r["host_monotonic_ns"] <= stop]
    inner = [r["payload"] for r in rows if r["event"] == "guest" and r["payload"]["event"] == "inner_sample"
             and start <= r["host_monotonic_ns"] <= stop]
    pre = phases["pre_capture"]["pss_kib"]
    held = statistics.median(r["pss_kib"] for r in hold)
    arm = verified["arm"]
    assert len(hold) >= 15 and len(inner) >= 10
    assert stop - start >= 5e9
    assert not any(r["event"] == "request" and start < r["host_monotonic_ns"] < stop for r in rows)
    if path.stem.endswith("-host"):
        aries = path.with_name(path.stem.removesuffix("-host") + "-replay.jsonl")
        timeline = [json.loads(line) for line in aries.read_text().splitlines()]
        source_waits = [r["requested_ns"] for r in timeline if r["event"] == "wait_start"]
        replies = sorted((r for r in timeline if r["event"] == "tool_reply"), key=lambda r: r["sequence"])
        exits = [r["exit_code"] for r in replies]
    else:
        source_waits = [r["requested_ns"] for r in rows if r["event"] == "source_wait"]
        observed = [r["actual_ns"] for r in rows if r["event"] == "source_wait_end"]
        assert all(actual >= requested for actual, requested in zip(observed, source_waits, strict=True))
        exits = [r["exit"] for r in rows if r["event"] == "tool_reply"]
    assert source_waits == [6068000000, 89202000000, 2873000000, 4360000000]
    assert verified["reward"]["overall_score"] == 1.0
    assert exits == [0, 0, 0, 128, 0]
    probe = [r["payload"]["response"] for r in rows if r["event"] == "guest"
             and r["payload"]["event"] == "reply"
             and "token" in r["payload"].get("response", {})]
    assert len(probe) == 2 and probe[0]["token"] == probe[1]["token"]
    assert probe[0]["inner_pid"] == probe[1]["inner_pid"] == 100
    assert (probe[0]["pid"] == probe[1]["pid"]) == (arm == "control")
    assert all(any(p["pid"] == probe[0]["pid"] for p in s["processes"]) == (arm == "control") for s in inner)
    assert all(s["worker_pid"] == (probe[0]["pid"] if arm == "control" else None) for s in inner)
    capture_peak = max(r["pss_kib"] for r in host
                       if phases["pre_capture"]["host_monotonic_ns"] <= r["host_monotonic_ns"] <= start)
    restore_peak = max(r["pss_kib"] for r in host
                       if stop <= r["host_monotonic_ns"] <= phases["post_restore"]["host_monotonic_ns"])
    snapshot = [r["payload"]["response"] for r in rows if r["event"] == "guest"
                and "checkpoint_disk_bytes" in r["payload"].get("response", {})]

    template_memory = []
    if arm == "checkpoint":
        checkpoint, = snapshot
        assert checkpoint["mode"] == "DeltaCR warm-template plus DeltaFS"
        assert probe[1]["restore_path"] == "warm-template" and probe[1]["workspace_rollback"]
        for sample in inner:
            template, = [p for p in sample["processes"] if p["pid"] == checkpoint["template_pid"]]
            assert template["state"] == "T" and sample["template_pid"] == template["pid"]
            template_memory.append(template["pss_kib"])

    def delta_ms(start_key, stop_key):
        return (verified[stop_key] - verified[start_key]) / 1e6

    return dict(arm=arm, receipt=path.name, hold_host_samples=len(hold), hold_guest_samples=len(inner),
                host_pss_pre_mib=pre / 1024, host_pss_hold_median_mib=held / 1024,
                host_pss_post_mib=phases["post_restore"]["pss_kib"] / 1024,
                host_pss_peak_capture_mib=capture_peak / 1024, host_pss_peak_restore_mib=restore_peak / 1024,
                host_pss_peak_during_cr_mib=max(capture_peak, restore_peak) / 1024,
                host_pss_released_mib=(pre - held) / 1024, host_pss_released_percent=(pre - held) * 100 / pre,
                inner_process_pss_hold_mib=statistics.median(sum(p["pss_kib"] for p in s["processes"]) for s in inner) / 1024,
                inner_template_pss_hold_mib=statistics.median(template_memory) / 1024 if template_memory else None,
                snapshot=snapshot[0] if snapshot else None,
                checkpoint_to_release_ms=delta_ms("checkpoint_start_ns", "release_ns") if arm == "checkpoint" else None,
                hold_ms=delta_ms("hold_start_ns", "hold_end_ns"),
                token_read_ms=delta_ms("restore_ns", "token_read_ns"),
                first_batch_ms=delta_ms("first_batch_start_ns", "first_batch_end_ns"),
                restore_to_first_batch_reply_ms=delta_ms("restore_ns", "first_batch_end_ns"),
                initial_wait_ms=source_waits[0] / 1e6,
                end_to_end_ms=delta_ms("arm_start_ns", "replay_end_ns"))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("arms", type=Path, nargs="+")
    args = parser.parse_args()
    arms = [summarize(path) for path in args.arms]
    assert len(arms) % 2 == 0
    contrasts = []
    for i in range(0, len(arms), 2):
        comparison = {r["arm"]: r for r in arms[i:i+2]}
        contrasts.append({key: comparison["checkpoint"][key] - comparison["control"][key]
                          for key in ("host_pss_released_mib", "first_batch_ms", "end_to_end_ms")})
    aggregate = {}
    for key in contrasts[0]:
        values = [pair[key] for pair in contrasts]
        aggregate[key] = {"median": statistics.median(values), "range": [min(values), max(values)]}
    print(json.dumps(dict(arms=arms, pairs=contrasts, paired_differences=aggregate, preliminary=len(contrasts) < 3), indent=2))
