"""Index original source IDs, observed tool types and live replay receipts."""
import argparse
import json
from pathlib import Path


def make(sources, inventory, reviewed, outputs):
    inspected = {(r["task"], r["trial"]): r for r in inventory}
    bound = {(w["task"], t["trial"]) for w in reviewed for t in w["trials"]}
    review_file = outputs / "full-replay-exclusions.json"
    exclusions = {(r["task"], r["trial"]): r for r in json.loads(review_file.read_text())} if review_file.exists() else {}
    unsafe_goals = {"break-filter-js-from-html", "crack-7z-hash"}
    require_review = {
        "break-filter-js-from-html": "Recorded task asks for a sanitization bypass.",
        "crack-7z-hash": "Recorded task asks for password cracking.",
        "git-leak-recovery": "Review each original command and any synthetic recovered secret before admitting a live credential-remediation workflow.",
        "fix-code-vulnerability": "Original command sequence lacks a review isolating defensive code edits from vulnerability reproduction.",
        "filter-js-from-html": "Original command sequence lacks a review isolating defensive filtering from executable attack tests.",
        "protein-assembly": "Fluorescent fusion work awaits source-specific review of substrates, original calls, and external dependencies.",
        "dna-assembly": "Fluorescent and tag marker assembly awaits source-specific review of substrates, original calls, and external dependencies.",
    }
    rows = []
    for workload in sources["workloads"]:
        task = workload["task"]
        for trial in workload["trials"]:
            ident = trial["id"]
            path = outputs / f"{task}-{ident}.json"
            item = inspected.get((task, ident))
            row = {"task": task, "trial": ident, "cohort": "pilot" if task == "fix-git" else "main",
                   "source_status": trial["status"], "reviewed_dispatch": (task, ident) in bound}
            if item:
                row["source_tool_counts"] = item["tool_counts"]
            attempts = sorted((outputs / "attempts").glob(f"{task}-{ident}-*.json"))
            if attempts:
                row["attempts"] = [str(p.relative_to(outputs)) for p in attempts]
            sparql_path = outputs / "sparql" / (ident+".json")
            if task == "sparql-university" and sparql_path.exists():
                proof = json.loads(sparql_path.read_text())
                row["source_record"] = str(sparql_path.relative_to(outputs))
                if proof.get("replay"):
                    live = proof["replay"]
                    row.update(receipt=row["source_record"], external_actions=proof["external_actions"],
                               live_guest_calls=len(live["tools"]),
                               state="validated_guest_exits_external_response_unavailable" if
                               live["session_completed"] and live["all_observed_exits_match"] else "partial_or_divergent")
                    rows.append(row)
                    continue
            if path.exists():
                r = json.loads(path.read_text())
                row["receipt"] = path.name
                row["state"] = ("validated_observed_exits" if r["session_completed"] and
                                r["all_observed_tool_exits_match"] else "partial_or_divergent")
                if r.get("external_actions"):
                    row["external_actions"] = r["external_actions"]
                    if row["state"] == "validated_observed_exits":
                        row["state"] = "validated_guest_exits_external_response_unavailable"
                if r.get("all_observed_image_payloads_match") is False:
                    row["state"] = "completed_guest_calls_image_payload_diverged" if r["session_completed"] else "partial_or_divergent"
                row["live_guest_calls"] = len(r["tools"])
            elif attempts:
                row["state"] = "live_attempt_incomplete_or_divergent"
            elif row["reviewed_dispatch"]:
                row["state"] = "source_bound_awaiting_validation"
            elif (task, ident) in exclusions:
                review = exclusions[task, ident]
                row.update(state=review["state"], reason=review["reason"], reviewed_source_steps=review["source_steps"])
            elif task in require_review:
                row.update(state="excluded_unsafe_goal" if task in unsafe_goals else "source_review_pending",
                           reason=require_review[task])
            else:
                ops = sorted(set(item["tool_counts"]) - {"exec_command", "update_plan"}) if item else []
                row.update(state="unsupported_or_unreviewed",
                           reason="Original trial includes " + ", ".join(ops) if ops else
                                  "Source-specific semantics and starting state await validation.")
            rows.append(row)
    return {"schema": "aries.harbor.trial-coverage.v1", "row": sources["row"], "job": sources["job"],
            "scope": "Uninstrumented replay validation. Fetch and metadata inspection alone do not credit a run.",
            "trials": rows}


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    for key in ("sources", "inventory", "reviewed", "outputs"):
        p.add_argument(key, type=Path)
    a = p.parse_args()
    out = make(*(json.loads(path.read_text()) for path in (a.sources, a.inventory, a.reviewed)), a.outputs)
    tmp = a.outputs / "coverage.json.tmp"
    tmp.write_text(json.dumps(out, indent=2)+"\n")
    tmp.replace(a.outputs / "coverage.json")
