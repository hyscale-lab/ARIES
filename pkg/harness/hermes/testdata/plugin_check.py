"""Drives the ARIES plugin's file operations inside the pinned Hermes image,
against the fake client in fake_aries_grpc.py. Run by
TestPluginFileOperationsInThePinnedImage; plain asserts, no pytest."""
import importlib.util
import json
import os

spec = importlib.util.spec_from_file_location("aries_plugin", "/plugin/__init__.py")
plugin = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plugin)
plugin.CLIENT = "/fake/aries-grpc"


class Context:
    def register_terminal_environment_provider(self, provider):
        self.provider = provider


context = Context()
plugin.register(context)
os.makedirs("/work/sub", exist_ok=True)
env = context.provider.create_environment(cwd="/work", timeout=30)
assert env.cwd == "/work", env.cwd

# The seam's own wiring is proven against the real bridge in
# TestPluginRunsHermesToolsThroughTheGRPCBridge; here the adapter is taken
# from the environment the way the seam takes it.
ops = env.get_file_operations()
assert type(ops).__name__ == "AriesFileOperations", type(ops)

executed = []
original = env.execute
env.execute = lambda command, *a, **k: executed.append(command) or original(command, *a, **k)
log = os.environ["FAKE_CLIENT_LOG"]


def client_entries():
    with open(log) as handle:
        return [json.loads(line) for line in handle]


def client_calls():
    return [entry["argv"] for entry in client_entries()]


def fresh():
    executed.clear()
    open(log, "w").close()


# write_file: the bytes land through `file write`, not the atomic-write script.
fresh()
result = ops.write_file("/work/sub/a.txt", "one\ntwo\nthree")
assert result.error is None, result
assert open("/work/sub/a.txt", "rb").read() == b"one\ntwo\nthree"
assert ["file", "write", "--size", "13", "/work/sub/a.txt"] in client_calls(), client_calls()
assert not any("mktemp" in command or "cat >" in command for command in executed), executed

# A BOM and CRLF line endings survive a rewrite, decided from typed probes.
open("/work/bom.txt", "wb").write(b"\xef\xbb\xbfa\r\nb\r\n")
result = ops.write_file("/work/bom.txt", "x\ny\n")
assert result.error is None, result
assert open("/work/bom.txt", "rb").read() == b"\xef\xbb\xbfx\r\ny\r\n", open("/work/bom.txt", "rb").read()

# Reads must match stock Hermes exactly: ShellFileOperations over a local
# environment on the same files is the oracle.
from tools.environments.local import LocalEnvironment  # noqa: E402
from tools.file_operations import ShellFileOperations  # noqa: E402
stock = ShellFileOperations(LocalEnvironment(cwd="/work"), cwd="/work")
samples = {
    "/work/lf.txt": b"one\ntwo\nthree\n",
    "/work/nolf.txt": b"one\ntwo\nthree",
    "/work/blank-tail.txt": b"a\n\n\n",
    "/work/empty.txt": b"",
    "/work/crlf.txt": b"a\r\nb\r\n",
    "/work/bom-read.txt": b"\xef\xbb\xbfhead\nbody\n",
    "/work/long.txt": ("x" * 5000 + "\nshort\n").encode(),
}
for name, data in samples.items():
    open(name, "wb").write(data)
for name in list(samples) + ["/work/sub", "/work/missing.txt", "/"]:
    for window in [(1, 2000), (2, 1), (1, 2), (3, 5), (9, 3)]:
        fresh()
        mine, theirs = ops.read_file(name, *window).to_dict(), stock.read_file(name, *window).to_dict()
        assert mine == theirs, (name, window, mine, theirs)
        assert executed == [] or name == "/work/missing.txt", (name, executed)
    assert ops.read_file_raw(name).to_dict() == stock.read_file_raw(name).to_dict(), name

# read_file_raw and read_file_bytes: whole content, no shell.
fresh()
assert ops.read_file_raw("/work/sub/a.txt").content == "one\ntwo\nthree"
open("/work/blob.bin", "wb").write(bytes(range(256)))
import base64  # noqa: E402
assert base64.b64decode(ops.read_file_bytes("/work/blob.bin").base64_content) == bytes(range(256))
assert executed == [], executed

# Binary, directory and missing paths keep Hermes's own results.
assert ops.read_file("/work/blob.bin").is_binary
assert ops.read_file("/work/sub").error
# The container root is a directory: stock Hermes gives its not-regular error.
assert "Cannot read '/': not a regular file" in ops.read_file("/").error, ops.read_file("/").error
assert ops.read_file_bytes("/work/none").error == "File not found: /work/none"

# The client gets the id of the tool call it serves from the ContextVar that
# Hermes sets per tool call, also on the thread that runs a command, and no
# variable outside a tool call.
from tools.approval import _approval_tool_call_id  # noqa: E402
token = _approval_tool_call_id.set("call_1")
fresh()
ops.read_file("/work/sub/a.txt")
env.execute("true")
_approval_tool_call_id.reset(token)
entries = client_entries()
assert {entry["argv"][0] for entry in entries} == {"exec", "file"}, entries
assert all(entry["tool_call_id"] == "call_1" for entry in entries), entries
fresh()
ops.read_file("/work/sub/a.txt")
env.execute("true")
assert all(entry["tool_call_id"] is None for entry in client_entries()), client_entries()

# A Hermes without the ContextVar fails the plugin at load, not per call.
import tools.approval  # noqa: E402
del tools.approval._approval_tool_call_id
try:
    plugin.register(Context())
except ImportError:
    pass
else:
    raise AssertionError("register() accepted a Hermes without the tool call id ContextVar")
finally:
    tools.approval._approval_tool_call_id = _approval_tool_call_id

# A broken client surfaces as an error, never as a shell fallback.
plugin.CLIENT = "/nonexistent/aries-grpc"
try:
    ops._sample_file_bytes("/work/sub/a.txt")
except Exception:
    pass
else:
    raise AssertionError("a missing client did not raise")

# A stalled client is killed at the file-call timeout and raises. A short
# terminal timeout keeps the check fast. The stall outlasts the file-call
# timeout many times, so an unbounded call returns empty instead of raising.
env.timeout = 1
with open("/fake/stall", "w") as handle:
    handle.write(f"#!/bin/sh\necho $$ > /tmp/stall.pid\nexec sleep {10 * env.timeout * plugin.FILE_TIMEOUT_FACTOR}\n")
os.chmod("/fake/stall", 0o755)
plugin.CLIENT = "/fake/stall"
try:
    ops._sample_file_bytes("/work/sub/a.txt")
except RuntimeError as error:
    assert str(error) == "aries-grpc-client file read timed out after 3s", error
else:
    raise AssertionError("a stalled client did not time out")
try:
    os.kill(int(open("/tmp/stall.pid").read()), 0)
except ProcessLookupError:
    pass
else:
    raise AssertionError("the stalled client still runs")
print("PLUGIN_CHECK_OK")
