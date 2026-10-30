"""ARIES terminal backend for Hermes.

The plugin runs commands and file calls through the ARIES gRPC tool bridge.
File tools use the seam in BaseEnvironment.get_file_operations().
Nothing is synced into the task sandbox.
"""

import base64
import os
import posixpath
import threading
from contextlib import contextmanager

try:
    import grpc
except ImportError:
    grpc = None
if grpc is not None:
    from . import sandbox_pb2 as pb
    from . import sandbox_pb2_grpc

FILE_CHUNK_BYTES = 64 << 10
MAX_MESSAGE_BYTES = 64 << 20
TRANSPORT_FAILURE_EXIT = 255
# A typed file call has a timeout of the Hermes terminal timeout multiplied by
# this factor. One call can run several Docker or Kubernetes operations in the
# sandbox (for example access check, transfer and rename), but Hermes gives
# each single shell command only the terminal timeout.
FILE_TIMEOUT_FACTOR = 3


def register(ctx):
    # Imported here so an incompatible Hermes fails this plugin with one log
    # line instead of breaking the loader.
    from agent.terminal_env_provider import TerminalEnvironmentProvider
    from tools.binary_extensions import BINARY_EXTENSIONS
    from tools.environments.base import BaseEnvironment, _ThreadedProcessHandle
    from tools.file_operations import (
        ExecuteResult, ReadResult, ShellFileOperations, _detect_line_ending, _strip_bom,
        describe_binary_file, normalize_read_pagination,
    )
    from tools.tool_output_limits import get_max_line_length
    # Hermes binds the id of the running tool call here, also on the threads
    # that run a command, and leaves it empty outside a tool call.
    from tools.approval import _approval_tool_call_id

    grpc_target = ctx.get_config("grpc_target")

    def call_metadata():
        tool_call_id = _approval_tool_call_id.get()
        return (("aries-tool-call-id", tool_call_id),) if tool_call_id else ()

    def rpc_error(error):
        code = error.code().name.title().replace("_", "")
        return f"aries: rpc error: code = {code} desc = {error.details()}\n"

    class AriesFileOperations(ShellFileOperations):
        """Reads, probes and the atomic write go through the bridge's typed
        file procedures; the tool methods above them (syntax gate, lint,
        patching, formatting) are Hermes's own. Still on the shell: search,
        patch reads, delete, move, the similar-name suggestions, the UTF-16
        rescue, and write_file's sha256 check and lint.

        A failure that Hermes would otherwise read as "no data" raises
        instead, so a broken bridge surfaces as a tool error and never as a
        silent fallback to the shell."""

        @contextmanager
        def _file_call(self, operation):
            timeout = self.env.timeout * FILE_TIMEOUT_FACTOR
            try:
                yield timeout, call_metadata()
            except grpc.RpcError as error:
                if error.code() == grpc.StatusCode.DEADLINE_EXCEEDED:
                    raise RuntimeError(f"aries: file {operation} timed out after {timeout}s") from None
                raise RuntimeError(f"aries: {error.details()}") from error

        def _absolute(self, path):
            path = self._expand_path(path)
            return path if posixpath.isabs(path) else posixpath.join(self.env.cwd, path)

        def _stat(self, path):
            with self._file_call("stat") as (timeout, metadata):
                reply = self.env._client().Stat(pb.StatRequest(path=path), timeout=timeout, metadata=metadata)
            if reply.exists and (reply.type not in (pb.FILE_TYPE_REGULAR, pb.FILE_TYPE_DIRECTORY, pb.FILE_TYPE_OTHER)
                                 or reply.size < 0):
                raise RuntimeError("aries: invalid stat reply")
            return reply

        def _read(self, path, max_bytes=0):
            header, chunks = None, []
            with self._file_call("read") as (timeout, metadata):
                stream = self.env._client().ReadFile(pb.ReadFileRequest(path=path, max_bytes=max_bytes),
                                                     timeout=timeout, metadata=metadata)
                for message in stream:
                    part = message.WhichOneof("part")
                    if part == "header" and header is None:
                        header = message.header
                    elif part == "chunk" and header is not None:
                        chunks.append(message.chunk)
                    else:
                        raise RuntimeError("aries: invalid read stream")
            if header is None:
                raise RuntimeError("aries: stream ended without a header")
            content = b"".join(chunks)
            if (header.size < len(content) or
                    (not header.truncated and len(content) != header.size) or
                    (header.truncated and (not max_bytes or len(content) != max_bytes or len(content) >= header.size)) or
                    (max_bytes and len(content) > max_bytes)):
                raise RuntimeError("aries: invalid read stream")
            return header, content

        def _head(self, path, length):
            """The first length bytes, or None when the file is absent."""
            try:
                return self._read(self._absolute(path), length)[1]
            except RuntimeError as error:
                if isinstance(error.__cause__, grpc.RpcError) and error.__cause__.code() == grpc.StatusCode.NOT_FOUND:
                    return None
                raise

        def _read_all(self, path):
            return self._read(path)[1]

        def _read_lines(self, path, offset, limit):
            summary, chunks = None, []
            with self._file_call("lines") as (timeout, metadata):
                stream = self.env._client().ReadLines(pb.ReadLinesRequest(
                    path=path, first_line=offset, max_lines=limit, max_line_bytes=4 * get_max_line_length() + 1),
                    timeout=timeout, metadata=metadata)
                for message in stream:
                    part = message.WhichOneof("part")
                    if part == "chunk" and summary is None:
                        chunks.append(message.chunk)
                    elif part == "summary" and summary is None:
                        summary = message.summary
                    else:
                        raise RuntimeError("aries: invalid line stream")
            if summary is None:
                raise RuntimeError("aries: stream ended without a summary")
            if summary.size < 0 or summary.total_lines < 0:
                raise RuntimeError("aries: invalid line stream")
            return summary, b"".join(chunks)

        def _write(self, path, data):
            def requests():
                yield pb.WriteFileRequest(header=pb.WriteFileHeader(path=path, size=len(data)))
                for offset in range(0, len(data), FILE_CHUNK_BYTES):
                    yield pb.WriteFileRequest(chunk=data[offset:offset + FILE_CHUNK_BYTES])

            with self._file_call("write") as (timeout, metadata):
                reply = self.env._client().WriteFile(requests(), timeout=timeout, metadata=metadata)
            if reply.bytes_written != len(data):
                raise RuntimeError("aries: invalid write reply")
            return reply

        def _is_binary(self, path, sample):
            return os.path.splitext(path)[1].lower() in BINARY_EXTENSIONS or self._is_likely_binary_bytes(sample)

        def _sample_file_bytes(self, path, length=1000):
            return self._head(path, length) or b""

        def _file_has_bom(self, path, pre_content=None):
            head = self._head(path, 3)
            return bool(head) and head.startswith(b"\xef\xbb\xbf")

        def _detect_file_line_ending(self, path, pre_content=None):
            if pre_content:
                return _detect_line_ending(pre_content)
            head = self._head(path, 4096)
            return _detect_line_ending(head.decode("utf-8", "replace")) if head else None

        def _atomic_write(self, path, content):
            data = content.encode("utf-8", "surrogateescape")
            self._write(self._absolute(path), data)
            return ExecuteResult(stdout="", exit_code=0)

        def read_file_bytes(self, path, max_bytes=None):
            path = self._absolute(path)
            stat = self._stat(path)
            if not stat.exists:
                return ReadResult(error=f"File not found: {path}")
            if stat.type != pb.FILE_TYPE_REGULAR:
                return self._not_regular_error(path)
            if max_bytes is not None and stat.size > max_bytes:
                return ReadResult(file_size=stat.size,
                                  error=f"File is too large ({stat.size:,} bytes, limit is {max_bytes:,})")
            content = self._read_all(path)
            return ReadResult(base64_content=base64.b64encode(content).decode(), file_size=stat.size, is_binary=True)

        def read_file_raw(self, path):
            path = self._absolute(path)
            stat = self._stat(path)
            if not stat.exists:
                return self._suggest_similar_files(path)
            if stat.type != pb.FILE_TYPE_REGULAR:
                return self._not_regular_error(path)
            if self._is_image(path):
                return ReadResult(is_image=True, is_binary=True, file_size=stat.size)
            sample = self._sample_file_bytes(path)
            if self._is_binary(path, sample):
                return ReadResult(is_binary=True, file_size=stat.size, error=describe_binary_file(sample, stat.size))
            text, _ = _strip_bom(self._read_all(path).decode("utf-8", "replace"))
            return ReadResult(content=text, file_size=stat.size)

        def read_file(self, path, offset=1, limit=2000):
            path = self._absolute(path)
            offset, limit = normalize_read_pagination(offset, limit)
            stat = self._stat(path)
            if not stat.exists:
                variant = self._unicode_variant_match(path)
                if variant is not None:
                    result = self.read_file(variant, offset=offset, limit=limit)
                    note = (f"Note: '{path}' not found byte-for-byte; resolved to the unicode-equivalent "
                            f"file '{variant}' (invisible encoding difference: NFC/NFD or special space/quote characters).")
                    result.hint = f"{note} {result.hint}" if result.hint else note
                    return result
                return self._suggest_similar_files(path)
            if stat.type != pb.FILE_TYPE_REGULAR:
                return self._not_regular_error(path)
            size = stat.size
            if self._is_image(path):
                return ReadResult(is_image=True, is_binary=True, file_size=size, hint=(
                    "Image file detected. Automatically redirected to vision_analyze tool. "
                    "Use vision_analyze with this file path to inspect the image contents."))
            sample = self._sample_file_bytes(path)
            if self._is_binary(path, sample):
                rescued = self._try_read_utf16(path, offset, limit, size)
                if rescued is not None:
                    return rescued
                return ReadResult(is_binary=True, file_size=size, error=describe_binary_file(sample, size))

            window, content = self._read_lines(path, offset, limit)
            text = content.decode("utf-8", "replace")
            if offset == 1:
                text, _ = _strip_bom(text)
            # Hermes's own rule, kept exactly: an untruncated window of a file
            # with no final newline loses its last newline.
            if not window.more and not window.ends_with_newline and text.endswith("\n"):
                text = text[:-1]
            total, end_line = window.total_lines, offset + limit - 1
            if size == 0:
                return ReadResult(content="", total_lines=0, file_size=0, hint="File is empty (0 bytes).")
            if offset > total > 0:
                return ReadResult(content="", total_lines=total, file_size=size, hint=(
                    f"Note: offset {offset} is beyond the end of the file ({total} lines total). "
                    f"Retry with offset <= {total}."))
            hint = None
            if window.more:
                hint = f"Use offset={end_line + 1} to continue reading (showing {offset}-{end_line} of {total} lines)"
            return ReadResult(content=self._add_line_numbers(text, offset), total_lines=total,
                              file_size=size, truncated=window.more, hint=hint)

    # The name must not contain ssh, docker, singularity, modal, daytona or
    # local: Hermes classifies environments by class-name substring before it
    # reads the stamped backend name.
    class AriesEnvironment(BaseEnvironment):
        def __init__(self, cwd, timeout):
            super().__init__(cwd=cwd, timeout=timeout)
            self._channel = None
            self._channel_lock = threading.Lock()
            self.init_session()

        def _client(self):
            with self._channel_lock:
                if self._channel is None:
                    if not isinstance(grpc_target, str) or not grpc_target.strip():
                        raise RuntimeError("gRPC target is not set")
                    self._channel = grpc.insecure_channel(grpc_target, options=(
                        ("grpc.enable_http_proxy", 0),
                        ("grpc.max_send_message_length", MAX_MESSAGE_BYTES),
                        ("grpc.max_receive_message_length", MAX_MESSAGE_BYTES),
                    ))
                return sandbox_pb2_grpc.SandboxStub(self._channel)

        def _run_bash(self, cmd_string, *, login=False, timeout=120, stdin_data=None):
            # Hermes bounds the call and kills this handle when the command times out.
            request = pb.ExecRequest(script=cmd_string,
                                     stdin=stdin_data.encode("utf-8", "surrogateescape") if stdin_data is not None else b"",
                                     login=login)
            try:
                call = self._client().Exec.future(request, metadata=call_metadata())
            except Exception as error:
                message = f"aries: {error}\n"
                return _ThreadedProcessHandle(lambda: (message, TRANSPORT_FAILURE_EXIT))

            def execute():
                try:
                    reply = call.result()
                except grpc.FutureCancelledError:
                    return "aries: rpc error: code = Canceled desc = context canceled\n", TRANSPORT_FAILURE_EXIT
                except grpc.RpcError as error:
                    return rpc_error(error), TRANSPORT_FAILURE_EXIT
                output = reply.stdout + reply.stderr
                if reply.truncated:
                    output += b"aries: output truncated at the bridge's limit\n"
                if reply.reason != pb.REASON_COMPLETED:
                    output += f"aries: {pb.Reason.Name(reply.reason)}\n".encode()
                return output.decode("utf-8", "replace"), reply.exit_code

            return _ThreadedProcessHandle(execute, cancel_fn=call.cancel)

        def cleanup(self):
            if self._channel is not None:
                self._channel.close()

        def get_file_operations(self):
            return AriesFileOperations(self)

    class AriesProvider(TerminalEnvironmentProvider):
        name = "aries"

        def is_available(self):
            return grpc is not None and isinstance(grpc_target, str) and bool(grpc_target.strip())

        def create_environment(self, *, cwd, timeout, task_id="default", image=None,
                               container_config=None, **kwargs):
            return AriesEnvironment(cwd=cwd, timeout=timeout)

    ctx.register_terminal_environment_provider(AriesProvider())
