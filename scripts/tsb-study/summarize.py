#!/usr/bin/env python3
"""Update the failed-gate study's status from retained mock/lifecycle evidence."""
import argparse
import collections
import json
from pathlib import Path

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--root",type=Path,required=True)
    args=ap.parse_args();base=args.root
    p=base/"study-status.json";status=json.loads(p.read_text())
    mocks=[]
    for root in sorted((base/"mock").iterdir()):
        result_path=root/"run-result.json"
        if not result_path.exists():continue
        r=json.loads(result_path.read_text());t=r["tasks"][0]
        host=[json.loads(line) for line in (root/"research-host.jsonl").read_text().splitlines()]
        samples=[x for x in host if x.get("type")=="host_sample"]
        configurations=set()
        for x in samples:
            if x["status"]=="measured_partial":
                m=x["observation"]["machine_configuration"]
                configurations.add((x["runtime_id"],m["vcpu_count"],m["mem_size_mib"]))
        response=t["harness"].get("final_response","")
        mocks.append({
            "path":str(root),"run_id":r["run_id"],"harness":t["harness"]["status"],
            "isolation":t["isolation"],"evaluation_score":t["evaluation"]["score"],
            "evaluation_status":t["evaluation"]["status"],
            "task_cleanup":t["cleanup"]["status"],
            "shared_cleanup":r["infrastructure"]["cleanup"]["status"],
            "HTTPS200":"HTTPS_STATUS 200" in response,
            "background_alive":"BACKGROUND_ALIVE" in response,
            "tool_diagnostic_error":"curl missing" if "curl: not found" in response else None,
            "host_samples_measured_partial":sum(x["status"]=="measured_partial" for x in samples),
            "host_samples_error":sum(x["status"]=="error" for x in samples),
            "machine_configurations":[dict(zip(["runtime_id","vcpu_count","memory_mib"],row)) for row in sorted(configurations)],
        })
    status["infrastructure"]["mock_attempts"]=mocks
    status["infrastructure"]["mock"]="Inspect per-attempt fields; reward zero is expected for diagnostic commands."
    status["infrastructure"]["mock_retries"]=max(0,len(mocks)-1)
    status["step_telemetry"]="16 Node fixture checks and native plugin registration pass; proposed source patch unbuilt; no benchmark Step evidence"
    status["host_accounting"]="VMM and shim mapping fields only; no total-host-footprint denominator"
    checks={}
    for line in (base/"validation/status.txt").read_text().splitlines():
        name,code=line.split();checks[name]=int(code)
    status["validation_exit_codes"]=checks
    status["infrastructure"]["native_lifecycle_test"]="passed" if checks.get("vhive-lifecycle")==0 else "unverified_or_failed"
    status["status"]="measurement_gate_failed"
    p.write_text(json.dumps(status,indent=2)+"\n")
    print("Recorded",len(mocks),"mock attempts; real stages remain unadmitted.")
if __name__=="__main__":main()
