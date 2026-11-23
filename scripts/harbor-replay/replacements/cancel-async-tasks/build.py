"""Pin five reviewed asyncio recordings and build an isolated replay adapter."""
import argparse
from datetime import datetime
import gzip
import hashlib
from io import BytesIO
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile


BASE = "f3b0a67"
TASK = "cancel-async-tasks"
JOB = "17d1a7f6-3339-4670-8b70-3b145979f57f"
INITIAL = (
    'test ! -e /app/run.py && test ! -e /app/.git\n'
    'test "$(python --version)" = "Python 3.13.15"\n'
    'command -v git patch\n'
    '! command -v apply_patch\n'
    'python --version'
)
INPUT_ARCHIVE = "36ae1b315f30cab6e9b06c4e81d7cb8e512658a2b799108b9080ff1b6999a74c"
PINNED = {
    "7be2f099-1162-4d7d-8b8c-50393b1b3968": ("c45dd6748e87627fb855a2a67a48dbe4c2ae5fd3a5b6710b9e1dfb229eb27e1e", "9db5e27aac34fea6a78a4466c01985c25a26257cf1ae8087acc369dabfc09732"),
    "a924d8be-b040-4fcc-aa20-ecb3b685ac9f": ("4fb0d13fe0bd553adb248be72be238e9b55560b2398f62f1acc0e8a24519e577", "1690355f6f63ad09d8457b43379034f8598bdd931fc50bed1a12049c128cc517"),
    "b4e5be65-a253-4086-bc35-819d746f9549": ("c19ca499dbe67493876f33496b86e9f1fb16360815f177ad5fc90238c73823f4", "cc79db3b27519510154cf05c0c9478fa85b133a6ae1cea402606e6cba38e0f93"),
    "de962f2d-96db-426a-8598-4570785024da": ("a539ab6f0f17dfb453cee1ae7cd6e52e4e75bd63a64a0bd6a8d4e6e23839ffe4", "6c6a6c474f2f46e084cbedad254030c05c5fd8ef79026daf3147f02358a16680"),
    "f265ccca-0b2f-47e1-aaa5-b5e17e5e2950": ("585995c1029c958d567f490c34c1c3133a43bf0dac3535b4218f6cfc14352d10", "8ec5aa223a5bd7386368477608ca66f01ae66b3e308227fcc309298dd2a73dc0"),
}
PATCH_INTERCEPTS = {
    "call_IH0I6DrE4iCoLhclXsahe8kW", "call_oFYcVvkryZfnuvY5vdn0OayX",
    "call_JtQY0ureycNKyzWNYycsEHg6", "call_mloffFKKAVYhjiVvGheA57gv",
}


def read(path):
    return json.loads(path.read_bytes())


def sha(path):
    with path.open("rb") as f:
        return hashlib.file_digest(f, "sha256").hexdigest()


def checked_edit(path, needle, replacement):
    text = path.read_text()
    assert text.count(needle) == 1, path
    path.write_text(text.replace(needle, replacement))


def packages(old, new):
    assert sha(old) == INPUT_ARCHIVE
    packed = BytesIO()
    with tarfile.open(old, "r:gz") as source, tarfile.open(fileobj=packed, mode="w") as target:
        for info in sorted(source.getmembers(), key=lambda f: f.name):
            assert info.isfile() and "/" not in info.name and info.name.endswith(".deb")
            if info.name.startswith(("curl_", "libcurl4_")):
                continue
            data = source.extractfile(info).read()
            clean = tarfile.TarInfo(info.name)
            clean.mode, clean.size = 0o644, len(data)
            target.addfile(clean, BytesIO(data))
    new.write_bytes(gzip.compress(packed.getvalue(), mtime=0))
    return sha(new)


def review(row, source, image, tools):
    items = read(row)
    assert {item["id"] for item in items} == set(PINNED)
    durations = sorted((datetime.fromisoformat(entry["finished_at"]) -
                        datetime.fromisoformat(entry["started_at"]), entry["id"]) for entry in items)
    median = durations[2][1]
    work = dict(task=TASK, image=image, vcpus=1, memory_mib=2048, storage_mib=10240, workdir="/app",
                tools=["exec_command", "update_plan", "web_search_call"], needs_network=False,
                effects=["local Python asyncio scheduling, test subprocesses and /app/run.py"], trials=[])
    entries = []
    for item in items:
        trial = item["id"]
        assert item["job_id"] == JOB and item["reward"] == 1 and item["status"] == "completed"
        folder = source/trial
        trajectory, rollout = folder/"trajectory.json", folder/"rollout.jsonl"
        assert (sha(trajectory), sha(rollout)) == PINNED[trial]
        record = read(trajectory)
        expected_files = []
        for step in record["steps"]:
            for call in step.get("tool_calls", []):
                assert call["function_name"] in work["tools"]
                if call["function_name"] != "exec_command":
                    continue
                cmd = call["arguments"]["cmd"]
                observations = step["observation"]["results"]
                output, = [r["content"] for r in observations if r.get("source_call_id") == call["tool_call_id"]]
                if ("*** Add File: " in cmd and
                        output.split("Output:")[-1].lstrip().startswith("Success. Updated the following files:")):
                    content = cmd.split("*** Add File: ", 1)[1].split("\n*** End Patch", 1)[0].splitlines()
                    assert content[0] in ("run.py", "/app/run.py") and all(line.startswith("+") for line in content[1:])
                    expected_files.append("\n".join(line[1:] for line in content[1:]) + "\n")
        artifact, = expected_files
        work["trials"].append(dict(trial=trial, trajectory_sha256=PINNED[trial][0], rollout_sha256=PINNED[trial][1]))
        entries.append(dict(trial=trial, source_wall_s=next(t.total_seconds() for t, uid in durations if uid==trial),
                            expected_run_py_sha256=hashlib.sha256(artifact.encode()).hexdigest()))
    return work, dict(task=TASK, job_id=JOB, image=image, package_bundle_sha256=tools,
                      deep_source=median, trials=entries)


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("output", type=Path)
    cli.add_argument("--row", type=Path, required=True)
    cli.add_argument("--sources", type=Path, required=True)
    cli.add_argument("--image", type=Path, required=True)
    cli.add_argument("--package-archive", type=Path, required=True)
    cli.add_argument("--diagnostic", action="store_true")
    args = cli.parse_args()
    out = args.output.resolve()
    out.mkdir()
    tools = packages(args.package_archive, out/"cancel-async-debs.tar.gz")
    image = read(args.image/"image.json")
    assert sha(args.image/"task.oci.tar") == image["archive_sha256"]
    reviewed, spec = review(args.row, args.sources, image["image"], tools)
    for name in ("task.oci.tar", "image.json"):
        shutil.copy2(args.image/name, out/name)
    git_root = Path(__file__).resolve().parents[4]
    spec["adapter_base"] = subprocess.check_output(["git", "rev-parse", BASE], cwd=git_root, text=True).strip()
    with tempfile.TemporaryDirectory(prefix="zb-cancel-build-") as temp:
        tree = Path(temp)
        tar = subprocess.check_output(["git", "archive", spec["adapter_base"]], cwd=git_root)
        with tarfile.open(fileobj=BytesIO(tar)) as archive:
            archive.extractall(tree, filter="data")
        (tree/"pkg/replay/harbor/reviewed.json").write_text(json.dumps([reviewed], indent=2)+"\n")
        insert = "var interceptedPatchDirs = map[string]string{"
        bindings = "".join(f'\n\t"{call}": "/app",' for call in sorted(PATCH_INTERCEPTS))
        checked_edit(tree/"pkg/replay/harbor/trajectory.go", insert, insert+bindings)
        insert = "var taskSetups = map[string]taskSetup{"
        setup = f'\n\t"{TASK}": {{packagesSHA: "{tools}", packagesField: "debian_task_tools_sha256", output: "run.py", initialCheck: `{INITIAL}`}},'
        checked_edit(tree/"cmd/aries-harbor/prepare.go", insert, insert+setup)
        if args.diagnostic:
            from diagnostic_overlay import apply
            apply(tree, checked_edit)
        subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-o", out/"aries-cancel-async-tasks",
                        "./cmd/aries-harbor"], cwd=tree, check=True)
    spec["diagnostic"] = args.diagnostic
    spec["adapter_sha256"] = sha(out/"aries-cancel-async-tasks")
    spec["image_metadata"] = image
    (out/"source-contract.json").write_text(json.dumps(spec, indent=2)+"\n")
    (out/"sources").mkdir()
    for item in spec["trials"]:
        trial = item["trial"]
        folder = out/"sources"/trial
        folder.mkdir()
        for f in ("trajectory.json", "rollout.jsonl", "lock.json", "provenance.json"):
            shutil.copy2(args.sources/trial/f, folder/f)
        check = subprocess.check_output([out/"aries-cancel-async-tasks", "-trajectory", folder/"trajectory.json",
                                         "-rollout", folder/"rollout.jsonl"])
        record = json.loads(check)
        assert (record["trial_id"], record["image"], record["workdir"]) == (trial, image["image"], "/app")
        (folder/"parsed-source.json").write_bytes(check)


if __name__ == "__main__":
    main()
