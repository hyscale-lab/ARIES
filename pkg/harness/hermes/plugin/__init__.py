"""ARIES terminal backend for Hermes.

Registers the `aries` backend, selected by TERMINAL_ENV=aries. Every command
runs through the staged ARIES gRPC client, which carries it to the task
sandbox over the per-task bridge; the client reads its target and credentials
from ARIES_GRPC_*. Nothing is synced into the sandbox: this backend never
constructs a FileSyncManager.

File tools reach the environment through BaseEnvironment.get_file_operations(),
the seam ARIES adds to the pinned Hermes before it starts.
"""

import base64
import json
import os
import posixpath
import subprocess

CLIENT = "/run/aries/bin/aries-grpc"
CLIENT_ENV = ("ARIES_GRPC_TARGET", "ARIES_GRPC_IDENTITY", "ARIES_GRPC_TRUSTED")


def register(ctx):
    # Imported here so an incompatible Hermes fails this plugin with one log
    # line instead of breaking the loader.
    from agent.terminal_env_provider import TerminalEnvironmentProvider
    from tools.binary_extensions import BINARY_EXTENSIONS
    from tools.environments.base import BaseEnvironment
    from tools.file_operations import (
        ExecuteResult, ReadResult, ShellFileOperations, _detect_line_ending, _strip_bom,
        describe_binary_file, normalize_read_pagination,
    )
    from tools.tool_output_limits import get_max_line_length

    class AriesFileOperations(ShellFileOperations):
        """Reads, probes and the atomic write go through the bridge's typed
        file procedures; the tool methods above them (syntax gate, lint,
        patching, formatting) are Hermes's own. Still on the shell: search,
        patch reads, delete, move, the similar-name suggestions, the UTF-16
        rescue, and write_file's sha256 check and lint.

        A failure that Hermes would otherwise read as "no data" raises
        instead, so a broken bridge surfaces as a tool error and never as a
        silent fallback to the shell."""

        def _file(self, *args, data=None, accept=(0,)):
            proc = subprocess.run([CLIENT, "file", *args], input=data, capture_output=True)
            if proc.returncode not in accept:
                message = proc.stderr.decode("utf-8", "replace").strip()
                raise RuntimeError(message or f"aries-grpc file {args[0]} exited {proc.returncode}")
            return proc

        def _absolute(self, path):
            path = self._expand_path(path)
            return path if posixpath.isabs(path) else posixpath.join(self.env.cwd, path)

        def _head(self, path, length):
            """The first length bytes, or None when the file is absent."""
            proc = self._file("read", "--max-bytes", str(length), self._absolute(path), accept=(0, 1))
            return proc.stdout if proc.returncode == 0 else None

        def _stat(self, path):
            return json.loads(self._file("stat", path).stdout)

        def _read_all(self, path, size):
            content = self._file("read", path).stdout
            if len(content) < size:
                raise RuntimeError(f"{path} is larger than the bridge returns in one read")
            return content

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
            proc = self._file("write", self._absolute(path), data=content.encode("utf-8", "surrogateescape"),
                              accept=range(256))
            return ExecuteResult(stdout=proc.stderr.decode("utf-8", "replace").strip(), exit_code=proc.returncode)

        def read_file_bytes(self, path, max_bytes=None):
            path = self._absolute(path)
            stat = self._stat(path)
            if not stat["exists"]:
                return ReadResult(error=f"File not found: {path}")
            if stat["type"] != "regular":
                return self._not_regular_error(path)
            if max_bytes is not None and stat["size"] > max_bytes:
                return ReadResult(file_size=stat["size"],
                                  error=f"File is too large ({stat['size']:,} bytes, limit is {max_bytes:,})")
            content = self._read_all(path, stat["size"])
            return ReadResult(base64_content=base64.b64encode(content).decode(), file_size=stat["size"], is_binary=True)

        def read_file_raw(self, path):
            path = self._absolute(path)
            stat = self._stat(path)
            if not stat["exists"]:
                return self._suggest_similar_files(path)
            if stat["type"] != "regular":
                return self._not_regular_error(path)
            if self._is_image(path):
                return ReadResult(is_image=True, is_binary=True, file_size=stat["size"])
            sample = self._sample_file_bytes(path)
            if self._is_binary(path, sample):
                return ReadResult(is_binary=True, file_size=stat["size"], error=describe_binary_file(sample, stat["size"]))
            text, _ = _strip_bom(self._read_all(path, stat["size"]).decode("utf-8", "replace"))
            return ReadResult(content=text, file_size=stat["size"])

        def read_file(self, path, offset=1, limit=2000):
            path = self._absolute(path)
            offset, limit = normalize_read_pagination(offset, limit)
            stat = self._stat(path)
            if not stat["exists"]:
                variant = self._unicode_variant_match(path)
                if variant is not None:
                    result = self.read_file(variant, offset=offset, limit=limit)
                    note = (f"Note: '{path}' not found byte-for-byte; resolved to the unicode-equivalent "
                            f"file '{variant}' (invisible encoding difference: NFC/NFD or special space/quote characters).")
                    result.hint = f"{note} {result.hint}" if result.hint else note
                    return result
                return self._suggest_similar_files(path)
            if stat["type"] != "regular":
                return self._not_regular_error(path)
            size = stat["size"]
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

            proc = self._file("lines", "--first", str(offset), "--max", str(limit),
                              "--max-line-bytes", str(4 * get_max_line_length() + 1), path)
            window = json.loads(proc.stderr)
            text = proc.stdout.decode("utf-8", "replace")
            if offset == 1:
                text, _ = _strip_bom(text)
            # Hermes's own rule, kept exactly: an untruncated window of a file
            # with no final newline loses its last newline.
            if not window["more"] and not window["ends_with_newline"] and text.endswith("\n"):
                text = text[:-1]
            total, end_line = window["total_lines"], offset + limit - 1
            if size == 0:
                return ReadResult(content="", total_lines=0, file_size=0, hint="File is empty (0 bytes).")
            if offset > total > 0:
                return ReadResult(content="", total_lines=total, file_size=size, hint=(
                    f"Note: offset {offset} is beyond the end of the file ({total} lines total). "
                    f"Retry with offset <= {total}."))
            hint = None
            if window["more"]:
                hint = f"Use offset={end_line + 1} to continue reading (showing {offset}-{end_line} of {total} lines)"
            return ReadResult(content=self._add_line_numbers(text, offset), total_lines=total,
                              file_size=size, truncated=window["more"], hint=hint)

    # The name must not contain ssh, docker, singularity, modal, daytona or
    # local: Hermes classifies environments by class-name substring before it
    # reads the stamped backend name.
    class AriesEnvironment(BaseEnvironment):
        def __init__(self, cwd, timeout):
            super().__init__(cwd=cwd, timeout=timeout)
            self.init_session()

        def _run_bash(self, cmd_string, *, login=False, timeout=120, stdin_data=None):
            # Hermes enforces the timeout itself by killing this process; the
            # closed connection then cancels the call on the bridge.
            argv = [CLIENT, "exec"] + (["--login"] if login else []) + ["--", cmd_string]
            proc = subprocess.Popen(
                argv,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                stdin=subprocess.PIPE if stdin_data is not None else subprocess.DEVNULL,
                text=True,
                encoding="utf-8",
                errors="replace",
            )
            if stdin_data is not None:
                # The client reads all of stdin before it writes anything, so a
                # blocking write cannot deadlock against its output.
                try:
                    proc.stdin.write(stdin_data)
                    proc.stdin.close()
                except BrokenPipeError:
                    pass  # the client exited early; its exit code says why
            return proc

        def cleanup(self):
            pass  # the bridge and the sandbox belong to ARIES, not to Hermes

        def get_file_operations(self):
            return AriesFileOperations(self)

    class AriesProvider(TerminalEnvironmentProvider):
        name = "aries"

        def is_available(self):
            return os.access(CLIENT, os.X_OK) and all(os.environ.get(key) for key in CLIENT_ENV)

        def create_environment(self, *, cwd, timeout, task_id="default", image=None,
                               container_config=None, **kwargs):
            return AriesEnvironment(cwd=cwd, timeout=timeout)

    ctx.register_terminal_environment_provider(AriesProvider())
