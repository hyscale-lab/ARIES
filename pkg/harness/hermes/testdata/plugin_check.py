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


def client_calls():
    with open(log) as handle:
        return [json.loads(line) for line in handle]


def fresh():
    executed.clear()
    open(log, "w").close()


# write_file: the bytes land through `file write`, not the atomic-write script.
fresh()
result = ops.write_file("/work/sub/a.txt", "one\ntwo\nthree")
assert result.error is None, result
assert open("/work/sub/a.txt", "rb").read() == b"one\ntwo\nthree"
assert ["file", "write", "/work/sub/a.txt"] in client_calls(), client_calls()
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
for name in list(samples) + ["/work/sub", "/work/missing.txt"]:
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
assert ops.read_file_bytes("/work/none").error == "File not found: /work/none"

# A broken client surfaces as an error, never as a shell fallback.
plugin.CLIENT = "/nonexistent/aries-grpc"
try:
    ops._sample_file_bytes("/work/sub/a.txt")
except Exception:
    pass
else:
    raise AssertionError("a missing client did not raise")
print("PLUGIN_CHECK_OK")
