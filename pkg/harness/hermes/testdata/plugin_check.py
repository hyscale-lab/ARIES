"""Check the ARIES plugin against a local gRPC bridge in the Hermes image."""
import importlib.util
import os
import sys

spec = importlib.util.spec_from_file_location("aries_plugin", "/plugin/__init__.py")
plugin = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = plugin
spec.loader.exec_module(plugin)
from fake_aries_grpc import CHUNK_BYTES, start
sandbox, server, target = start()
os.environ["ARIES_GRPC_TARGET"] = target


class Context:
    def register_terminal_environment_provider(self, provider):
        self.provider = provider


context = Context()
plugin.register(context)
os.makedirs("/work/sub", exist_ok=True)
env = context.provider.create_environment(cwd="/work", timeout=30)
assert env.cwd == "/work", env.cwd
assert context.provider.is_available()
loaded_grpc = plugin.grpc
plugin.grpc = None
assert not context.provider.is_available()
plugin.grpc = loaded_grpc
from tools.environments.base import _ThreadedProcessHandle
handle = env._run_bash("printf ported", stdin_data="input")
assert isinstance(handle, _ThreadedProcessHandle), type(handle)
assert handle.wait(10) == 0, handle.returncode
assert handle.stdout.read() == "ported"
channel = env._channel
assert channel is not None

# The seam's own wiring is proven against the real bridge in
# TestPluginRunsHermesToolsThroughTheGRPCBridge; here the adapter is taken
# from the environment the way the seam takes it.
ops = env.get_file_operations()
assert type(ops).__name__ == "AriesFileOperations", type(ops)

executed = []
original = env.execute
env.execute = lambda command, *a, **k: executed.append(command) or original(command, *a, **k)


def fresh():
    executed.clear()
    sandbox.calls.clear()


# write_file: the bytes land through `file write`, not the atomic-write script.
fresh()
result = ops.write_file("/work/sub/a.txt", "one\ntwo\nthree")
assert result.error is None, result
assert open("/work/sub/a.txt", "rb").read() == b"one\ntwo\nthree"
assert env._channel is channel
assert any(op == "write" and fields == {"path": "/work/sub/a.txt", "size": len(b"one\ntwo\nthree")}
           for op, _, fields in sandbox.calls), sandbox.calls
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

# File streams keep exact bytes and send one chunk per bounded message.
large = bytes(range(256)) * (3 * CHUNK_BYTES // len(bytes(range(256))) + 1)
fresh()
assert ops._file("write", "--size", str(len(large)), "/work/large.bin", data=large).returncode == 0
assert ops._file("read", "/work/large.bin").stdout == large
assert ops._file("read", "--max-bytes", str(len(large) // 2), "/work/large.bin").stdout == large[:len(large) // 2]
assert ops._file("write", "--size", str(len(large) + 1), "/work/large.bin", data=large,
                 accept=range(256)).returncode == 3
assert open("/work/large.bin", "rb").read() == large
assert ops._file("read", "/work/none", accept=(0, 1)).returncode == 1
assert ops._file("read", "/work/sub", accept=range(256)).returncode == 5
sandbox.summaryless = True
try:
    ops._file("lines", "--first", "1", "--max", "1", "/work/sub/a.txt")
except RuntimeError as error:
    assert "without a summary" in str(error), error
else:
    raise AssertionError("a line stream without a summary did not fail")
sandbox.summaryless = False

# The ContextVar supplies metadata for exec and file calls, not environment variables.
from tools.approval import _approval_tool_call_id  # noqa: E402
token = _approval_tool_call_id.set("call_1")
fresh()
ops.read_file("/work/sub/a.txt")
ops._file("write", "--size", str(len(b"x")), "/work/id.txt", data=b"x")
env.execute("true")
_approval_tool_call_id.reset(token)
assert {entry[0] for entry in sandbox.calls} >= {"exec", "stat", "read", "lines", "write"}, sandbox.calls
assert all(entry[1] == "call_1" for entry in sandbox.calls), sandbox.calls
fresh()
ops.read_file("/work/sub/a.txt")
env.execute("true")
assert all(entry[1] is None for entry in sandbox.calls), sandbox.calls

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

# A refused file call raises; absent files keep the read-head semantics.
assert ops._head("/work/none", len(b"probe")) is None
try:
    ops._file("stat", "relative")
except RuntimeError as error:
    assert "path must be absolute" in str(error), error
else:
    raise AssertionError("a rejected file call did not raise")

# A gRPC deadline cancels a stalled call and reports the file timeout.
env.timeout = 1
sandbox.stall = True
try:
    ops._sample_file_bytes("/work/sub/a.txt")
except RuntimeError as error:
    assert str(error) == "aries: file read timed out after 3s", error
else:
    raise AssertionError("a stalled bridge did not time out")
sandbox.stall = False

# The process handle preserves bytes, login, exit codes, and bridge notices.
from aries_plugin import sandbox_pb2 as pb
sandbox.response = pb.ExecResponse(stdout=b"out", stderr=b"err", exit_code=7,
                                   reason=pb.REASON_COMPLETED)
handle = env._run_bash("exit 7", login=True, stdin_data="piped")
assert handle.wait(10) == 7, handle.returncode
assert handle.stdout.read() == "outerr"
assert sandbox.calls[-1][2] == {"script": "exit 7", "stdin": b"piped", "login": True}, sandbox.calls[-1]
sandbox.response = pb.ExecResponse(stdout=b"out", stderr=b"err", exit_code=255,
                                   reason=pb.REASON_SANDBOX_ERROR, truncated=True)
handle = env._run_bash("true")
assert handle.wait(10) == plugin.TRANSPORT_FAILURE_EXIT
assert handle.stdout.read() == "outerraries: output truncated at the bridge's limit\naries: REASON_SANDBOX_ERROR\n"
sandbox.response = None

# A bridge refusal is a transport failure, not a sandbox exit code.
import grpc
sandbox.refusal = grpc.StatusCode.RESOURCE_EXHAUSTED
handle = env._run_bash("true")
assert handle.wait(10) == plugin.TRANSPORT_FAILURE_EXIT, handle.returncode
assert "aries: rpc error: code = ResourceExhausted desc = bridge refused the call\n" == handle.stdout.read()
assert ops._file("read", "/work/sub/a.txt", accept=range(256)).returncode == 4
sandbox.refusal = grpc.StatusCode.PERMISSION_DENIED
assert ops._file("read", "/work/sub/a.txt", accept=range(256)).returncode == 2
sandbox.refusal = None

# Cancellation reaches the server through the pending gRPC call.
sandbox.cancelled.clear()
sandbox.running.clear()
handle = env._run_bash("sleep 30")
assert sandbox.running.wait(10), "server did not start the command"
handle.kill()
assert sandbox.cancelled.wait(10), "server did not receive cancellation"
assert handle.wait(10) == plugin.TRANSPORT_FAILURE_EXIT, handle.returncode
assert "aries:" in handle.stdout.read()

# A revoked bridge returns 255 rather than a command exit code.
server.stop(0).wait()
handle = env._run_bash("true")
assert handle.wait(10) == plugin.TRANSPORT_FAILURE_EXIT
assert "aries:" in handle.stdout.read()

# A missing target reports a transport failure without starting a process.
env.cleanup()
env._channel = None
os.environ.pop("ARIES_GRPC_TARGET")
assert not context.provider.is_available()
handle = env._run_bash("true")
assert handle.wait(10) == plugin.TRANSPORT_FAILURE_EXIT, handle.returncode
assert "ARIES_GRPC_TARGET is not set" in handle.stdout.read()
print("PLUGIN_CHECK_OK")
