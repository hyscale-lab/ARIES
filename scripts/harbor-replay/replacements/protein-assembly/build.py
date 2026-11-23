"""Build a replay adapter for the reviewed 93c2 recording with digest-only output."""
import argparse
import hashlib
from io import BytesIO
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

from image import INPUTS, MANIFEST, REFERENCE


BASE = "f3b0a677b1675e46562d43fb21085ce66a0026ce"
TASK = "protein-assembly"
TRIAL = "93c2fb63-9bd5-4e9d-9024-06fbbc0c1087"
PAIR = dict(trajectory="23d96f5a985ccca07784adc300cb048ed83b8fa1ad0d0d7c156257be1d4e9496",
            rollout="bc259e5b9549919ed2e99933164627a457ce7c7464b222ca09de271ce49c662d")
OUTPUT = "ef918710402ce8bc544f39f88f88ca2fa2ee2a3df507b099bdcc8759291bf30b"
INPUT_DIGESTS = {
    "antibody.fasta": "e0e13645b299470cb007f1daf13050010bf15cb7c22ff6042189ea1ca7a1c333",
    "pdb_ids.txt": "ea338c4e1a776a338d7d302e80ceaee6379c1c85a031448851c07b0f764358c2",
    "plasmid.gb": "c9e31ad0ebb4d14b3820e46678c336d1f77af6ab1da36c53b5e9a9f10676cd74",
}
NATIVE = frozenset({"exec_command", "write_stdin", "web_search_call", "update_plan"})
PACKAGE_SHA = "d5724f0fa473fe60002710c9ae8d5e4e259392811e246168c5b0991d1d29db25"
TASK_LOCK = "sha256:c491d45acce234e1aa1d44dcc6778c55fcf875e9eb254b8dd3ae5ef58119621b"


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def edit(path, needle, replacement):
    s = path.read_text()
    if s.count(needle) != 1:
        raise ValueError(f"unexpected adapter base: {path.name}")
    path.write_text(s.replace(needle, replacement))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("output", type=Path)
    p.add_argument("--source", type=Path, required=True)
    p.add_argument("--image", type=Path, required=True)
    p.add_argument("--packages", type=Path, required=True)
    args = p.parse_args()
    out = args.output.resolve()
    out.mkdir()
    source = args.source
    for key, name in (("trajectory", "trajectory.json"), ("rollout", "rollout.jsonl")):
        if digest(source/name) != PAIR[key]:
            raise ValueError("source digest differs from the reviewed original")
    lock = json.loads((source/"lock.json").read_bytes())
    if lock["task"]["digest"] != TASK_LOCK:
        raise ValueError("unexpected upstream task lock")
    image = json.loads((args.image/"image.json").read_bytes())
    if (image["sha256_manifest"], image["input_sha256"], digest(args.image/"task.oci.tar")) != (
        MANIFEST, INPUT_DIGESTS, image["archive_sha256"]
    ):
        raise ValueError("unexpected benchmark image or initial inputs")
    if digest(args.packages) != PACKAGE_SHA:
        raise ValueError("unexpected package archive")
    original = json.loads((source/"trajectory.json").read_bytes())
    step = next(row for row in original["steps"] if row["step_id"] == 24)
    if OUTPUT not in step["observation"]["results"][0]["content"]:
        raise ValueError("recorded final digest is missing")
    reviewed = dict(
        task=TASK, workdir="/app", image=REFERENCE, needs_network=True,
        vcpus=1, memory_mib=4096, storage_mib=10240, tools=sorted(NATIVE),
        effects=["Fixed reviewed reporter benchmark, scoped read-only public dependencies",
                 "Guest command text and output bodies are excluded from measurement receipts"],
        trials=[dict(trial=TRIAL, trajectory_sha256=PAIR["trajectory"], rollout_sha256=PAIR["rollout"])],
    )
    git_root = Path(__file__).resolve().parents[4]
    with tempfile.TemporaryDirectory(prefix="zb-protein-build-") as tmp:
        tree = Path(tmp)
        snapshot = subprocess.check_output(["git", "archive", BASE], cwd=git_root)
        with tarfile.open(fileobj=BytesIO(snapshot)) as archive:
            archive.extractall(tree, filter="data")
        (tree/"pkg/replay/harbor/reviewed.json").write_text(json.dumps([reviewed], indent=2) + "\n")
        first = """test ! -e /app/gblock.txt
! command -v apply_patch
command -v python patch git
python --version
mkdir -m 0700 -p /tmp/codex-home"""
        checks = "sha256sum -c <<'ARIES_SHA'\n" + "\n".join(
            f"{digest}  {name}" for name, digest in sorted(INPUT_DIGESTS.items())
        ) + "\nARIES_SHA"
        if set(INPUT_DIGESTS) != INPUTS:
            raise ValueError("unexpected initial file list")
        insert = "var taskSetups = map[string]taskSetup{"
        body = f'\n\t"{TASK}": {{packagesSHA: "{PACKAGE_SHA}", packagesField: "debian_task_tools_sha256", output: "gblock.txt", initialCheck: `{first}`, inputCheck: `{checks}`}},'
        edit(tree/"cmd/aries-harbor/prepare.go", insert, insert + body)
        # Formatting blobs and source responses stay in memory for verification.
        # The private record includes only digests, timing, IDs and status.
        edit(tree/"cmd/aries-harbor/main.go",
             "func(e harbor.Receipt) error { return log.Encode(e) }",
             """func(e harbor.Receipt) error {
            e.Stdout = ""
            e.Stderr = ""
            return log.Encode(e)
        }""")
        # A transport failure can carry guest-provided diagnostic text.
        # The last completed lifecycle event identifies its stage.
        edit(tree/"cmd/aries-harbor/main.go", "fmt.Fprintln(os.Stderr, err)",
             'fmt.Fprintln(os.Stderr, "source-bound replay failed; inspect the last digest-only receipt")')
        subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-o",
                        out/"aries-protein-assembly", "./cmd/aries-harbor"], cwd=tree, check=True)
    private = out/"source"
    private.mkdir(mode=0o700)
    for name in ("trajectory.json", "rollout.jsonl", "lock.json", "provenance.json"):
        shutil.copy2(source/name, private/name)
    for name in ("task.oci.tar", "image.json"):
        shutil.copy2(args.image/name, out/name)
    shutil.copy2(args.packages, out/"task-debs.tar.gz")
    contract = dict(base=BASE, source=reviewed, task_lock=TASK_LOCK, image=image,
                    package_sha256=PACKAGE_SHA, output_sha256=OUTPUT,
                    adapter_sha256=digest(out/"aries-protein-assembly"),
                    full_initial_files_seen_in_source_step=6)
    (out/"source-contract.json").write_text(json.dumps(contract, indent=2) + "\n")


if __name__ == "__main__":
    main()
