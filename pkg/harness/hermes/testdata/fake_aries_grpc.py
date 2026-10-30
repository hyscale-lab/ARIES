"""Local fake gRPC bridge for the Hermes plugin check."""
import os
import subprocess
import threading
from concurrent import futures

import grpc
from aries_plugin import sandbox_pb2 as pb
from aries_plugin import sandbox_pb2_grpc

CHUNK_BYTES = 64 << 10


class Sandbox(sandbox_pb2_grpc.SandboxServicer):
    def __init__(self):
        self.calls = []
        self.cancelled = threading.Event()
        self.running = threading.Event()
        self.stall = False
        self.response = None
        self.refusal = None
        self.summaryless = False

    def record(self, context, operation, **fields):
        self.calls.append((operation, dict(context.invocation_metadata()).get("aries-tool-call-id"), fields))

    def Exec(self, request, context):
        self.record(context, "exec", script=request.script, stdin=request.stdin, login=request.login)
        if self.refusal is not None:
            context.abort(self.refusal, "bridge refused the call")
        if self.response is not None:
            return self.response
        command = ["bash", "-l" if request.login else "--norc", "-c", request.script]
        proc = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        context.add_callback(lambda: (self.cancelled.set(), proc.kill()) if proc.poll() is None else None)
        self.running.set()
        stdout, stderr = proc.communicate(request.stdin)
        return pb.ExecResponse(stdout=stdout, stderr=stderr, exit_code=proc.returncode, reason=pb.REASON_COMPLETED)

    def Stat(self, request, context):
        self.record(context, "stat", path=request.path)
        if not os.path.isabs(request.path):
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "path must be absolute")
        if not os.path.lexists(request.path):
            return pb.StatResponse()
        info = os.stat(request.path)
        kind = pb.FILE_TYPE_REGULAR if os.path.isfile(request.path) else pb.FILE_TYPE_DIRECTORY if os.path.isdir(request.path) else pb.FILE_TYPE_OTHER
        return pb.StatResponse(exists=True, type=kind, size=info.st_size if kind == pb.FILE_TYPE_REGULAR else 0, mode=info.st_mode & 0o7777)

    def read(self, path, context):
        if self.refusal is not None:
            context.abort(self.refusal, "bridge refused the call")
        if not os.path.isabs(path):
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "path must be absolute")
        if not os.path.exists(path):
            context.abort(grpc.StatusCode.NOT_FOUND, "no such file")
        if not os.path.isfile(path):
            context.abort(grpc.StatusCode.FAILED_PRECONDITION, "not a regular file")
        if self.stall:
            threading.Event().wait(10)
        with open(path, "rb") as handle:
            return handle.read()

    def ReadFile(self, request, context):
        self.record(context, "read", path=request.path, max_bytes=request.max_bytes)
        data = self.read(request.path, context)
        limit = request.max_bytes or len(data)
        yield pb.ReadFileResponse(header=pb.ReadFileHeader(size=len(data), truncated=limit < len(data)))
        content = data[:limit]
        for offset in range(0, len(content), CHUNK_BYTES):
            yield pb.ReadFileResponse(chunk=content[offset:offset + CHUNK_BYTES])

    def ReadLines(self, request, context):
        self.record(context, "lines", path=request.path, first=request.first_line, max=request.max_lines, max_line_bytes=request.max_line_bytes)
        data = self.read(request.path, context)
        parts = data.split(b"\n")
        lines = [part + b"\n" for part in parts[:-1]] + ([parts[-1]] if parts[-1] else [])
        for number, line in enumerate(lines, start=1):
            if request.first_line <= number < request.first_line + request.max_lines:
                body, newline = (line[:-1], b"\n") if line.endswith(b"\n") else (line, b"")
                body = body[:request.max_line_bytes] if request.max_line_bytes else body
                yield pb.ReadLinesResponse(chunk=body + newline)
        total = data.count(b"\n")
        if not self.summaryless:
            yield pb.ReadLinesResponse(summary=pb.ReadLinesSummary(total_lines=total, size=len(data), ends_with_newline=data.endswith(b"\n"), more=total > request.first_line + request.max_lines - 1))

    def WriteFile(self, requests, context):
        iterator = iter(requests)
        header = next(iterator).header
        self.record(context, "write", path=header.path, size=header.size)
        if not os.path.isabs(header.path):
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "path must be absolute")
        data = b"".join(message.chunk for message in iterator)
        if len(data) != header.size:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "write stream does not match its header")
        created = not os.path.exists(header.path)
        os.makedirs(os.path.dirname(header.path), exist_ok=True)
        with open(header.path, "wb") as target:
            target.write(data)
        return pb.WriteFileResponse(bytes_written=len(data), created=created)


def start():
    sandbox = Sandbox()
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    sandbox_pb2_grpc.add_SandboxServicer_to_server(sandbox, server)
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    return sandbox, server, f"127.0.0.1:{port}"
