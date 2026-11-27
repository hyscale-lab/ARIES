#!/usr/bin/env python3
"""Hold the first resume of each AgentENV file restore on a study-owned service.

Use as [firecracker] binary_path with COW_GATE_SOCKET and COW_GATE_FIRECRACKER
in the service environment. Run serve CONTROL RECEIPTS.jsonl under the service
user. list CONTROL exposes current VMMs. release CONTROL PID [PID ...] checks
every selected VMM, then sends all pending resumes in one event-loop callback.
Use a fresh control path and receipt file per coordinator, and real_socket for
independent host observations. Receipts use CLOCK_MONOTONIC in this host boot.
"""
import argparse
import asyncio
import json
import os
from pathlib import Path
import socket
import struct
import sys
from time import monotonic_ns


def control(path, request):
    with socket.socket(socket.AF_UNIX) as sock:
        sock.connect(path)
        sock.sendall(json.dumps(request).encode() + b"\n")
        with sock.makefile("rb") as reader:
            response = json.loads(reader.readline())
    if "error" in response:
        raise RuntimeError(response["error"])
    return response["result"]


def launch(args):
    binary = os.environ["COW_GATE_FIRECRACKER"]
    if "--api-sock" in args:
        index = args.index("--api-sock") + 1
        public = Path(args[index]).absolute()
        real = public.with_name(f"fc-{os.getpid()}.sock")
        control(os.environ["COW_GATE_SOCKET"], dict(action="register", public=str(public), real=str(real)))
        args[index] = str(real)
    os.execv(binary, [binary, *args])


def peer(writer):
    sock = writer.get_extra_info("socket")
    return struct.unpack("3i", sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))


async def http_message(reader):
    head = await reader.readuntil(b"\r\n\r\n")
    first, *headers = head[:-4].split(b"\r\n")
    fields = {k.lower(): v.strip() for k, v in (h.split(b":", 1) for h in headers)}
    if b"transfer-encoding" in fields:
        raise ValueError("expected AgentENV's fixed-length HTTP message")
    size = int(fields.get(b"content-length", 0))
    if not 0 <= size <= 32 * 1024 * 1024:
        raise ValueError(f"invalid message size {size}")
    return first, head, await reader.readexactly(size)


class Gate:
    def __init__(self, receipts):
        self.out = receipts.open("x", buffering=1)
        self.directory = receipts.parent
        self.vms = {}
        self.pending = {}
        self.release_lock = asyncio.Lock()
        self.record("coordinator", boot_id=Path("/proc/sys/kernel/random/boot_id").read_text().strip())

    def record(self, event, **fields):
        row = dict(event=event, monotonic_ns=monotonic_ns(), **fields)
        print(json.dumps(row), file=self.out)
        return row["monotonic_ns"]

    async def upstream(self, vm):
        reader, writer = await asyncio.open_unix_connection(vm["real_socket"])
        if peer(writer)[0] != vm["pid"]:
            writer.close()
            raise ValueError("stock socket peer PID differs from launcher")
        return reader, writer

    async def observe(self, vm):
        reader, writer = await self.upstream(vm)
        try:
            writer.write(b"GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
            status, _, data = await http_message(reader)
            info = json.loads(data)
            if status.split()[1] != b"200" or info.get("state") != "Paused":
                raise ValueError(f"expected Paused, got {status!r} {info!r}")
            return info
        finally:
            writer.close()

    async def proxy(self, vm, reader, client):
        pid = vm["pid"]
        upstream = None
        try:
            try:
                first, head, body = await http_message(reader)
            except asyncio.IncompleteReadError as exc:
                if exc.partial:
                    raise
                return  # AgentENV's connection-only socket readiness check
            method, path, _ = first.split()
            load = method == b"PUT" and path == b"/snapshot/load"
            resume = method == b"PATCH" and path == b"/vm" and json.loads(body)["state"] == "Resumed"
            if vm["state"] in ("loading", "confirming", "held", "releasing") and method != b"GET":
                raise ValueError("another VM mutation is pending")
            if load:
                params = json.loads(body)
                if (vm["state"] != "ready" or params.get("resume_vm") is not False
                        or params["mem_backend"]["backend_type"] != "File"):
                    raise ValueError("expected a fresh, paused File snapshot load")
                vm.update(state="loading", load=params)
            gate_resume = resume and vm["state"] == "loaded"
            if gate_resume:
                # Reserve the first resume before connecting; racing requests must see the hold.
                vm["state"] = "confirming"
            remote, upstream = await self.upstream(vm)
            if gate_resume:
                info = await self.observe(vm)
                released = asyncio.get_running_loop().create_future()
                self.pending[pid] = (reader, remote, upstream, head + body, released)
                vm["held_ns"] = self.record("held", pid=pid, info=info)
                vm["state"] = "held"
                await released  # release() sends the bytes from one callback
            else:
                upstream.write(head + body)
            status, response, data = await http_message(remote)
            code = int(status.split()[1])
            reply_ns = monotonic_ns()
            if load:
                vm["state"] = "loaded" if code == 204 else "failed"
                vm["load_done_ns"] = self.record("load_reply", pid=pid, status=code, reply_ns=reply_ns, load=params)
            if resume and vm["state"] == "releasing":
                vm["state"] = "resumed" if code == 204 else "failed"
                vm["resume_reply_ns"] = self.record("resume_reply", pid=pid, status=code, reply_ns=reply_ns)
            if method == b"GET" and path == b"/vm/dirty-memory-ranges" and code == 200:
                archive = f"dirty-{pid}-{reply_ns}.json"
                with (self.directory / archive).open("xb") as f:
                    f.write(data)
                self.record("dirty_ranges", pid=pid, reply_ns=reply_ns, archive=archive)
            # One request per front connection; tell hyper to open the next one.
            lines = [s for s in response[:-4].split(b"\r\n") if not s.lower().startswith(b"connection:")]
            client.write(b"\r\n".join(lines) + b"\r\nConnection: close\r\n\r\n" + data)
        except Exception as exc:
            self.record("proxy_error", pid=pid, error=repr(exc))
            msg = json.dumps({"fault_message": str(exc)}).encode()
            client.write(f"HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\nContent-Length: {len(msg)}\r\n\r\n".encode() + msg)
        finally:
            if upstream:
                upstream.close()
            client.close()

    async def register(self, pid, public, real):
        if pid in self.vms or not public.is_absolute() or real != public.with_name(f"fc-{pid}.sock"):
            raise ValueError("invalid or repeated VMM registration")
        if os.path.lexists(public) or os.path.lexists(real):
            raise FileExistsError(f"study socket already exists: {public} or {real}")
        pfd = os.pidfd_open(pid)
        vm = self.vms[pid] = dict(pid=pid, socket=str(public), real_socket=str(real), state="starting")
        self.record("registered", **vm)
        server = None
        loop = asyncio.get_running_loop()

        def exited():
            vm["state"] = "exited"
            if server:
                server.close()
            if pid in self.pending:
                self.pending.pop(pid)[-1].cancel()
            self.record("exited", pid=pid)
            self.vms.pop(pid)
            loop.remove_reader(pfd)
            os.close(pfd)

        async def start():
            nonlocal server
            try:
                while vm["state"] == "starting":
                    try:
                        _, writer = await self.upstream(vm)
                    except (FileNotFoundError, ConnectionRefusedError):
                        await asyncio.sleep(.01)
                        continue
                    writer.close()
                    if os.path.lexists(public):
                        raise FileExistsError(public)
                    server = await asyncio.start_unix_server(lambda r, w: self.proxy(vm, r, w), path=public)
                    vm["state"] = "ready"
            except Exception as exc:
                vm["state"] = "failed"
                self.record("listen_error", pid=pid, error=repr(exc))

        loop.add_reader(pfd, exited)
        asyncio.create_task(start())

    async def release(self, pids):
        async with self.release_lock:
            if not pids or len(set(pids)) != len(pids):
                raise ValueError("expected distinct held VMMs")
            vms = [self.vms[pid] for pid in pids]
            if any(vm["state"] != "held" for vm in vms):
                raise ValueError("every selected VMM must be held")
            infos = await asyncio.gather(*(self.observe(vm) for vm in vms))
            pending = [self.pending[pid] for pid in pids]
            if any(client.at_eof() or remote.at_eof() or writer.is_closing()
                   for client, remote, writer, _, _ in pending):
                raise ValueError("pending resume disconnected before release")
            for vm, info in zip(vms, infos):
                self.record("paused_before_release", pid=vm["pid"], info=info)
            barrier = monotonic_ns()
            # Preconnected sockets. Avoid disk I/O and scheduler yields between writes.
            for vm, (_, _, writer, request, released) in zip(vms, pending):
                vm["state"] = "releasing"
                vm["resume_send_ns"] = monotonic_ns()
                writer.write(request)
                vm["resume_sent_ns"] = monotonic_ns()
                released.set_result(None)
            for vm in vms:
                del self.pending[vm["pid"]]
                self.record("resume_sent", pid=vm["pid"], barrier_ns=barrier, send_ns=vm["resume_send_ns"],
                            sent_ns=vm["resume_sent_ns"])
            return {"barrier_ns": barrier, "pids": pids}

    async def command(self, reader, writer):
        try:
            pid, uid, _ = peer(writer)
            if uid not in (0, os.geteuid()):
                raise PermissionError("study coordinator user mismatch")
            request = json.loads(await reader.readline())
            if request["action"] == "register":
                result = await self.register(pid, Path(request["public"]), Path(request["real"]))
            elif request["action"] == "release":
                result = await self.release(request["pids"])
            elif request["action"] == "list":
                result = list(self.vms.values())
            else:
                raise ValueError("unknown coordinator action")
            answer = {"result": result}
        except Exception as exc:
            answer = {"error": repr(exc)}
        writer.write(json.dumps(answer).encode() + b"\n")
        writer.close()


async def serve(path, receipts):
    os.umask(0o077)
    if os.path.lexists(path):
        raise FileExistsError(path)
    gate = Gate(Path(receipts))
    server = await asyncio.start_unix_server(gate.command, path=path)
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    if sys.argv[1:2] not in (["serve"], ["list"], ["release"]) and "COW_GATE_FIRECRACKER" in os.environ:
        launch(sys.argv[1:])
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("serve", "list", "release"))
    parser.add_argument("control")
    parser.add_argument("extra", nargs="*")
    args = parser.parse_args()
    if args.action == "serve":
        if len(args.extra) != 1:
            parser.error("serve requires the path of a new receipt file")
        asyncio.run(serve(args.control, args.extra[0]))
    else:
        print(json.dumps(control(args.control, dict(action=args.action, pids=[int(p) for p in args.extra]))))
