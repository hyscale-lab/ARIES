"""Validate an unchanged source fixture and signed package snapshot by digest."""
from cohort_sparql import CODEX_TOOLS, OS_TOOLS

PROFILES = {
    "ddf7548f-de77-4e90-aa31-ae1f86311601": ("20260831T212241Z", "6840d29c7c6f5ab1c28b3be5cce0bada7a74cf978a496e6d5a632191922279fc"),
    "8968e176-fa1f-4d6a-8e5a-3ebd7cc0b8de": ("20260831T202034Z", "fee914d1d9a30cc385d25613ae53c0f3777ac79e4bc4ca01aedf66800fe18059"),
}


def evidence(events, receipt, connectivity, reference, count):
    trial = receipt["trial_id"]
    stamp, output_sha = PROFILES[trial]
    status = {e["phase"]: e["network"]["dependencies"]["snapshot.ubuntu.com"]["status"] for e in connectivity}
    if status != {"before_snapshot": 200, "after_restore": 200}:
        raise ValueError("guest lost its signed package endpoint")
    ready, = [e["state"] for e in events if e["event"]=="source_task_ready"]
    required = {f"URIs: https://snapshot.ubuntu.com/ubuntu/{stamp}/",
                "Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg", "Candidate: 2.6.1-4"}
    if not required <= {line.strip() for line in ready["checks"].splitlines()}:
        raise ValueError("source snapshot, keyring, or package version differs")
    if ready["codex_prefix_sha256"] != CODEX_TOOLS or ready["ubuntu_packages_sha256"] != OS_TOOLS:
        raise ValueError("source tooling archive differs")
    final, = [e["checks"] for e in events if e["event"]=="final_task_state"]
    if final.splitlines() != [output_sha+"  primers.fasta","sequences.fasta: OK","primers.fasta: OK"]:
        raise ValueError("final output or unchanged input differs")
    if not receipt["final_file_validation"]["verified"] or receipt["final_file_validation"]["expected_output_sha256"] != output_sha:
        raise ValueError("native validation did not establish the expected output")
    return dict(trial=trial, guest_calls=count, cni_network=events[0]["cni_network"], signed_snapshot=stamp,
                guest_https=status, final_output_sha256=output_sha, external_and_rejected_boundaries=reference["external"])
