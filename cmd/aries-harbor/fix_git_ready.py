"""Recreate Harbor's time-dependent Git setup in the freshly allocated benchmark VM."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time

START = time.monotonic_ns()
TARGET = b"7a0e1bb4e7816c63c3fd967dfb1990ccdc67d7c9"
INITIAL = b"d7d3e4ba9350f634d92d39ace2b471433ee57d50"
# Native source reflog and git show --format=fuller, 2026-08-31 20:21:51 UTC.
COMMIT_TIME = 1788207711


def git(*args, data=None):
    return subprocess.check_output(["git", *args], input=data)


if git("rev-parse", "HEAD").strip() != INITIAL or git("symbolic-ref", "HEAD").strip() != b"refs/heads/master" or git("status", "--porcelain"):
    raise ValueError("unexpected initial branch, commit or worktree")
head = Path(".git/logs/HEAD")
master = Path(".git/logs/refs/heads/master")
rows = head.read_bytes().splitlines()
if len(rows) != 5 or len(master.read_bytes().splitlines()) != 2:
    raise ValueError("unexpected initial reflog shape")
old = rows[3].split(b" ", 2)[1]
if not rows[3].endswith(b"\tcommit: Move to Stanford") or not rows[4].endswith(b"\tcheckout: moving from " + old + b" to master"):
    raise ValueError("unexpected detached commit or checkout")
original = git("cat-file", "commit", old)
payload, edits = re.subn(rb"(?m)^((?:author|committer) Test User <test@example.com>) \d+ \+0000$",
                         rb"\g<1> " + str(COMMIT_TIME).encode() + b" +0000", original)
if edits != 2 or git("hash-object", "-t", "commit", "--stdin", data=payload).strip() != TARGET:
    raise ValueError("recorded Git object cannot be reproduced from the prepared tree and parent")
parent = git("rev-parse", old + b"^").strip()
tree = git("rev-parse", old + b"^{tree}").strip()
if parent != git("rev-parse", "c4e38a1").strip():
    raise ValueError("unexpected parent")
before = git("reflog", "--all", "--date=iso")
if git("hash-object", "-w", "-t", "commit", "--stdin", data=payload).strip() != TARGET:
    raise ValueError("failed to write recovered object")

for path, times in [(head, [COMMIT_TIME-1]*3 + [COMMIT_TIME]*2), (master, [COMMIT_TIME-1]*2)]:
    rewritten = []
    for line, stamp in zip(path.read_bytes().splitlines(), times):
        text, count = re.subn(rb" \d+ \+0000\t", b" " + str(stamp).encode() + b" +0000\t", line.replace(old, TARGET))
        if count != 1:
            raise ValueError("unexpected reflog timestamp")
        rewritten.append(text + b"\n")
    tmp = path.with_suffix(".zb-prepare")
    with tmp.open("xb") as writer:
        writer.writelines(rewritten)
    os.replace(tmp, path)

after = git("reflog", "--all", "--date=iso")
expected_reflog = "ed135cb1cbe5739bce4a2b896d27b4ba74bcb3e59d411dda429dc4ac11d6925c"
if hashlib.sha256(after).hexdigest() != expected_reflog:
    raise ValueError("reconstructed reflog differs from the source")
reachable = git("rev-list", "--all", "--reflog").splitlines()
if TARGET not in reachable or (old != TARGET and old in reachable) or git("rev-parse", TARGET+b"^{tree}").strip() != tree or git("rev-parse", TARGET+b"^").strip() != parent:
    raise ValueError("reachable history, tree or parent differs after preparation")
print(json.dumps({"before_detached":old.decode(), "after_detached":TARGET.decode(), "head":INITIAL.decode(),
                  "tree":tree.decode(),"parent":parent.decode(),"reflog_before_sha256":hashlib.sha256(before).hexdigest(),
                  "reflog_after_sha256":expected_reflog,"guest_duration_ns":time.monotonic_ns()-START}))
