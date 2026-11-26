package main

const pretrainShardTask = "pretrain-shard-corruption"

var pretrainShardSetup = taskSetup{
	packagesSHA:   "2d1665bc3a472a5e895657f41612ecf4e40e4349757d6481a19420b57b8146d0",
	packagesField: "offline_patch_archive_sha256",
	output:        "DATA_RECOVERY.md",
	inputCheck:    "echo 'a00d393663bcef0dc8b061e9b8ab8b386cf32bf41860d191ed3542000365c6b4  data/val/val.bin' | sha256sum -c -",
	initialCheck: `test ! -e DATA_RECOVERY.md
test ! -e diagnostics
test ! -e checkpoints
test ! -e recover_train_data.py
test ! -e data/raw
test ! -e gen_shards.py
test ! -e /tmp/pretrain-investigation
test "$(command -v patch)" = /usr/bin/patch
dpkg-query -W git patch
sha256sum -c <<'ARIESSHARD'
15f944933821ee3025b40c5a37f1f6912cd187ce1ea3298151d32f1040146d0f  run_pretrain.sh
bfd145b4fca234f4009bded7ea607366459e0e803fe29ba7fa9e9142d764d099  train_pretrain.py
d5aa5c91725ec6fb026a80eba9c337c36bdddefb4cc2040227072c9bbd651545  validate_data.py
b70ede8b2b0ce957b64de20f6b7dfec815673352e4f58f228b6cff3b6f8e938a  requirements-debug.txt
ARIESSHARD
test -x run_pretrain.sh
export LC_ALL=C
test "$(sha256sum data/train/*.bin data/train/index.json data/val/val.bin | sha256sum)" = 'd2b64ca1cb4e6a5a53faec1f101b6aaeddd006082c97550d1b7e61cc68edb218  -'
df -P -B1 .
python - <<'PYSHARD'
import json
import struct
import sys
from importlib.metadata import version
from pathlib import Path

assert sys.version_info[:2] == (3, 11)
for name, expected in (("torch", "2.3.1+cpu"), ("litdata", "0.2.17"), ("numpy", "1.26.4"), ("sentencepiece", "0.2.0")):
    assert version(name) == expected
    print(name, expected)
root = Path("data/train")
index = json.loads((root / "index.json").read_bytes())
chunks = index["chunks"]
assert len(chunks) == 20
assert index["config"]["data_format"] == ["no_header_tensor:18"]
assert {p.name for p in root.glob("*.bin")} == {f"chunk-0-{i}.bin" for i in range(20)}
for chunk in chunks:
    assert (chunk["chunk_size"], chunk["dim"], chunk["chunk_bytes"]) == (162, 20898, 167840)
    path = root / chunk["filename"]
    assert path.stat().st_size == 167840
    with path.open("rb") as stream:
        assert struct.unpack("<164I", stream.read(656)) == (162, *range(656, 167841, 1032))
assert Path("data/val/val.bin").stat().st_size == 206400
print("train_chunks", 20, "validation_bytes", 206400)
PYSHARD
` + noShellPatch + "\n" + pretrainShardGit + "\n" + pretrainShardLoopback,
}

// Torch's localhost rendezvous advertises the VM hostname to both local ranks.
const pretrainShardLoopback = `python - <<'PYSHARDHOST'
from pathlib import Path
import socket

name = socket.gethostname()
assert name and all(c.isascii() and (c.isalnum() or c in ".-") for c in name), name
with Path("/etc/hosts").open("a") as hosts:
    hosts.write(f"\n127.0.0.1 localhost {name}\n")
for alias in ("localhost", name):
    address = socket.gethostbyname(alias)
    assert address == "127.0.0.1", (alias, address)
    print("Gloo rendezvous hostname:", alias, address)
PYSHARDHOST`

// Both recordings name these commits in later tools. Recreate the embedded
// repository from the verified worktree and its source commit dates.
const pretrainShardGit = `original_head=f780cde8722f46589f53d83eb745ae89ceb768e1
git diff --exit-code HEAD
if test "$(git rev-parse HEAD)" != "$original_head"; then
    fixture=$(mktemp -d)
    trap 'rm -rf -- "$fixture"' EXIT
    cp -p run_pretrain.sh train_pretrain.py validate_data.py requirements-debug.txt "$fixture/"
    git -C "$fixture" init -q -b master
    git -C "$fixture" config user.name "Claude Claudius"
    git -C "$fixture" config user.email "claude@lab.ai"
    (
        export GIT_COMMITTER_DATE=2026-08-05T06:48:09Z
        cd "$fixture"
        git add run_pretrain.sh train_pretrain.py validate_data.py
        git commit -qm "initial pretraining setup" --date=2025-01-07T09:00:00Z
        git add requirements-debug.txt
        git commit -qm "chore: bump torch 2.3.1 -> 2.4.0 in debug reqs" --date=2025-01-14T11:32:00Z
        git commit -q --allow-empty -m "wip: debug distributed hang, adding notes" --date=2025-01-14T16:45:00Z
        test "$(git rev-parse HEAD)" = "$original_head"
    )
    mv .git "$fixture/build-git"
    mv "$fixture/.git" .git
fi
test "$(git symbolic-ref HEAD)" = refs/heads/master
test "$(git rev-parse HEAD HEAD^ HEAD^^ | tr '\n' ' ')" = "$original_head cec0bb966ed609ed3db18ceb4777dc820e06f737 f3cece6d6801cadeb0ece5f7584302ea1bba2c51 "
test "$(git status --porcelain)" = '?? data/'
git log -3 --format=%H`

const pretrainShardFinal = `
test -s checkpoints/train_metrics.json
test -s checkpoints/model_final.pt
sha256sum checkpoints/train_metrics.json checkpoints/model_final.pt
python -c 'import json; print(json.load(open("checkpoints/train_metrics.json")))'`
