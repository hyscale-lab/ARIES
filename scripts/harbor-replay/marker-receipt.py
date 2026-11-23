"""Export content-free source fidelity and file digest evidence for the pinned marker fixture."""
import argparse
import hashlib
import json
from pathlib import Path
import runpy

OUTPUT_DIGESTS = {
    "ddf7548f-de77-4e90-aa31-ae1f86311601": "6840d29c7c6f5ab1c28b3be5cce0bada7a74cf978a496e6d5a632191922279fc",
    "8968e176-fa1f-4d6a-8e5a-3ebd7cc0b8de": "fee914d1d9a30cc385d25613ae53c0f3777ac79e4bc4ca01aedf66800fe18059",
}
INPUT_SHA = "ebdc361b5fba28eb0a2c7a8a8a444ccb12cc842c8cbc6df5a2c24254f130736f"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def receipt(log, source, host, binary, revision, exit_status, configuration):
    summarize = runpy.run_path(str(Path(__file__).with_name("validation-receipt.py")))["summarize"]
    result = summarize(log, source/"trajectory.json", host, "dna-assembly", revision, digest(binary.read_bytes()), configuration)
    if result["rollout_sha256"] != digest((source/"rollout.jsonl").read_bytes()):
        raise ValueError("original rollout digest differs")
    output_sha = OUTPUT_DIGESTS[result["trial_id"]]
    final = [r for r in result["source_setup_and_final_state"] if r["event"]=="final_task_state"]
    required = [output_sha + "  primers.fasta", "sequences.fasta: OK", "primers.fasta: OK"]
    verified = len(final)==1 and final[0]["checks"].splitlines()==required
    if exit_status == 0 and not (verified and result["session_completed"] and result["all_observed_tool_exits_match"]):
        raise ValueError("incomplete source or final digest verification")
    for key in ("final_tool_stdout","final_tool_stderr"):
        result[key+"_sha256"] = digest(result.pop(key).encode())
    result["external_actions"] = [
        {"step_id": r["step_id"], "response_status": r["response_status"]} for r in result["external_actions"]]
    result["source_setup_and_final_state"] = [
        {k:v for k,v in r.items() if k in ("event","begin_monotonic_ns","end_monotonic_ns")}
        for r in result["source_setup_and_final_state"]]
    result["final_file_validation"] = dict(verified=verified, expected_input_sha256=INPUT_SHA,
                                         expected_output_sha256=output_sha)
    result["driver_exit"] = exit_status
    return result


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    for key in ("log","source","binary"):
        parser.add_argument(key,type=Path)
    parser.add_argument("--host",required=True)
    parser.add_argument("--revision",required=True)
    parser.add_argument("--driver-exit",required=True,type=int)
    parser.add_argument("--configuration", required=True, choices=("v1","v2","v3","v4"))
    args=parser.parse_args()
    print(json.dumps(receipt(args.log,args.source,args.host,args.binary,args.revision,args.driver_exit,args.configuration),indent=2))
