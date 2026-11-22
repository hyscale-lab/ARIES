"""Index original source IDs, observed tool types and live replay receipts."""
import argparse
import json
from pathlib import Path


def make(sources, inventory, reviewed, outputs):
    inspected = {(r["task"], r["trial"]): r for r in inventory}
    bound = {(w["task"], t["trial"]) for w in reviewed for t in w["trials"]}
    held = {
        "break-filter-js-from-html": "Recorded task asks for a sanitization bypass.",
        "crack-7z-hash": "Recorded task asks for password cracking.",
        "git-leak-recovery": "Recorded task asks to recover secrets from Git history.",
        "fix-code-vulnerability": "Original command sequence lacks a review isolating defensive code edits from vulnerability reproduction.",
        "filter-js-from-html": "Original command sequence lacks a review isolating defensive filtering from executable attack tests.",
        "protein-assembly": "Original sequence engineering and synthesis work has not been admitted for live replay.",
        "dna-assembly": "Original sequence-specific assembly and primer design has not been admitted for live replay.",
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
            if path.exists():
                r = json.loads(path.read_text())
                row["receipt"] = path.name
                row["state"] = ("validated_observed_exits" if r["session_completed"] and
                                r["all_observed_tool_exits_match"] else "partial_or_divergent")
                row["live_guest_calls"] = len(r["tools"])
            elif row["reviewed_dispatch"]:
                row["state"] = "source_bound_awaiting_validation"
            elif task in held:
                row.update(state="metadata_only", reason=held[task])
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
