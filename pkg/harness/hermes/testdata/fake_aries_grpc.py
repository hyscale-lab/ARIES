#!/usr/bin/env python3
"""Stand-in for aries-grpc in the plugin test: the same argv, stdout, stderr
and exit-code contract, served from the local filesystem instead of a bridge.
Every invocation is appended to $FAKE_CLIENT_LOG."""
import json
import os
import subprocess
import sys

args = sys.argv[1:]
with open(os.environ["FAKE_CLIENT_LOG"], "a") as log:
    log.write(json.dumps(args) + "\n")

if args[0] == "exec":
    login = "--login" in args
    script = args[-1]
    sys.exit(subprocess.run(["bash", "-l" if login else "--norc", "-c", script]).returncode)

operation, rest = args[1], args[2:]
flags = dict(zip(rest[:-1:2], rest[1:-1:2]))
path = rest[-1]
if not os.path.isabs(path):
    print("path must be absolute", file=sys.stderr)
    sys.exit(3)
if operation == "stat":
    if not os.path.lexists(path):
        print(json.dumps({"exists": False, "type": "", "size": 0, "mode": "0000"}))
    else:
        info = os.stat(path)
        kind = "regular" if os.path.isfile(path) else "directory" if os.path.isdir(path) else "other"
        print(json.dumps({"exists": True, "type": kind, "size": info.st_size if kind == "regular" else 0, "mode": "%04o" % (info.st_mode & 0o7777)}))
    sys.exit(0)
if operation == "write":
    data = sys.stdin.buffer.read()
    created = not os.path.exists(path)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "wb") as target:
        target.write(data)
    print(json.dumps({"bytes_written": len(data), "created": created}), file=sys.stderr)
    sys.exit(0)
if not os.path.exists(path):
    print("no such file", file=sys.stderr)
    sys.exit(1)
if not os.path.isfile(path):
    print("not a regular file", file=sys.stderr)
    sys.exit(5)
data = open(path, "rb").read()
if operation == "read":
    offset = int(flags.get("--offset", 0))
    limit = int(flags.get("--max-bytes", 0)) or len(data)
    chunk = data[offset:offset + limit]
    sys.stdout.buffer.write(chunk)
    print(json.dumps({"size": len(data), "truncated": offset + len(chunk) < len(data)}), file=sys.stderr)
elif operation == "lines":
    first, count, clamp = int(flags["--first"]), int(flags["--max"]), int(flags.get("--max-line-bytes", 0))
    window = []
    parts = data.split(b"\n")
    lines = [part + b"\n" for part in parts[:-1]] + ([parts[-1]] if parts[-1] else [])
    for number, line in enumerate(lines, start=1):
        if first <= number < first + count:
            body, newline = (line[:-1], b"\n") if line.endswith(b"\n") else (line, b"")
            window.append((body[:clamp] if clamp else body) + newline)
    total = data.count(b"\n")
    sys.stdout.buffer.write(b"".join(window))
    print(json.dumps({"total_lines": total, "size": len(data), "ends_with_newline": data.endswith(b"\n"), "more": total > first + count - 1}), file=sys.stderr)
