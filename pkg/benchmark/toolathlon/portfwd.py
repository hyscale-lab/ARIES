"""Loopback forwarder for an ARIES Toolathlon sandbox.

Toolathlon's task-side code reaches its self-hosted applications at fixed
localhost ports because its own runner uses host networking. An ARIES task
container is on a private network, so this process listens on those ports
on loopback and carries each connection to the Docker host, where the
applications publish the same ports. Standard library only; it must run on
whatever Python the task image provides.
"""

import argparse
import asyncio
import socket
import struct
import sys


def default_gateway():
    """The container's default route, which is the Docker host on a bridge."""
    with open("/proc/net/route", encoding="ascii") as route_table:
        for line in route_table.readlines()[1:]:
            fields = line.split()
            if len(fields) >= 3 and fields[1] == "00000000":
                return socket.inet_ntoa(struct.pack("<L", int(fields[2], 16)))
    raise SystemExit("portfwd: no default route in /proc/net/route")


async def pipe(reader, writer):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
    except (ConnectionError, asyncio.CancelledError, asyncio.IncompleteReadError):
        pass
    finally:
        try:
            writer.close()
        except Exception:  # noqa: BLE001 - best-effort teardown
            pass


def make_handler(target, port):
    async def handle(client_reader, client_writer):
        try:
            upstream_reader, upstream_writer = await asyncio.open_connection(target, port)
        except OSError as exc:
            print(f"portfwd: {target}:{port} refused: {exc}", file=sys.stderr, flush=True)
            client_writer.close()
            return
        await asyncio.gather(
            pipe(client_reader, upstream_writer),
            pipe(upstream_reader, client_writer),
        )

    return handle


async def listen(handler, host, port):
    return await asyncio.start_server(handler, host, port)


async def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--target", required=True, help="host to forward to, or 'auto' for the default gateway")
    parser.add_argument("--ports", required=True, help="comma-separated TCP ports")
    parser.add_argument("--ready", required=True, help="file written once every port is bound")
    args = parser.parse_args()

    target = default_gateway() if args.target == "auto" else args.target
    ports = [int(port) for port in args.ports.split(",") if port]
    servers = []
    for port in ports:
        handler = make_handler(target, port)
        servers.append(await listen(handler, "127.0.0.1", port))
        # Node resolves `localhost` to ::1 first; bind it too when the
        # container has IPv6 loopback.
        try:
            servers.append(await listen(handler, "::1", port))
        except OSError:
            pass
    with open(args.ready, "w", encoding="ascii") as ready:
        ready.write(f"{target} {','.join(str(port) for port in ports)}\n")
    print(f"portfwd: forwarding {ports} to {target}", flush=True)
    await asyncio.gather(*(server.serve_forever() for server in servers))


if __name__ == "__main__":
    asyncio.run(main())
