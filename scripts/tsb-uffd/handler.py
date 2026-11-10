#!/usr/bin/env python3
"""Serve native Firecracker UFFD missing-page faults, retaining per-phase evidence."""
import argparse
import array
import ctypes
import errno
import fcntl
import json
import os
from pathlib import Path
import select
import signal
import socket
import struct
import time

PAGE = 4096
UFFD_EVENT_PAGEFAULT = 0x12
UFFD_EVENT_REMOVE = 0x15
UFFDIO_COPY = (3 << 30) | (40 << 16) | (0xAA << 8) | 3


def identity(pid):
    try:
        return Path(f'/proc/{pid}/stat').read_text().rsplit(')', 1)[1].split()[19]
    except FileNotFoundError:
        return None


def mapping_for(address, mappings):
    page = address & ~(PAGE - 1)
    for index, region in enumerate(mappings):
        base = region['base_host_virt_addr']
        if base <= page < base + region['size']:
            offset = region['offset'] + page - base
            result = {'region': index, 'host_page_address': page,
                      'snapshot_offset': offset, 'snapshot_page': offset // PAGE}
            if 'guest_base' in region:
                result['guest_page_address'] = region['guest_base'] + page - base
            return result
    raise ValueError(f'Fault address outside registered mappings: {address:#x}')


def validate_mappings(mappings, size, guest_bases=None):
    if not isinstance(mappings, list) or not mappings:
        raise ValueError('Empty/non-list mappings')
    if guest_bases is not None and len(guest_bases) != len(mappings):
        raise ValueError('Guest-base count differs from native mapping count')
    for i, region in enumerate(mappings):
        for name in ('base_host_virt_addr', 'size', 'offset'):
            if not isinstance(region.get(name), int) or region[name] < 0 or region[name] % PAGE:
                raise ValueError('Unaligned/noninteger mapping ' + name)
        if region.get('page_size', region.get('page_size_kib')) != PAGE:
            raise ValueError('Only native 4096-byte mappings are supported')
        if region['size'] == 0 or region['offset'] + region['size'] > size:
            raise ValueError('Mapping exceeds snapshot')
        if guest_bases is not None:
            base = guest_bases[i]
            if not isinstance(base, int) or base < 0 or base % PAGE:
                raise ValueError('Invalid guest base')
            region['guest_base'] = base
        for prior in mappings[:i]:
            for start, length in [('base_host_virt_addr', 'size'), ('offset', 'size')]:
                if max(region[start], prior[start]) < min(region[start]+region[length], prior[start]+prior[length]):
                    raise ValueError('Overlapping mappings')
    if sum(r['size'] for r in mappings) != size:
        raise ValueError('Mappings do not cover snapshot size')
    return mappings


def receive_handshake(conn):
    data, fds = bytearray(), []
    try:
        while True:
            chunk, ancillary, flags, _ = conn.recvmsg(65536, socket.CMSG_SPACE(16))
            if flags & (socket.MSG_CTRUNC | socket.MSG_TRUNC):
                raise ValueError('Truncated handshake')
            if not chunk:
                raise ValueError('EOF before complete handshake')
            data.extend(chunk)
            for level, kind, payload in ancillary:
                if level == socket.SOL_SOCKET and kind == socket.SCM_RIGHTS:
                    parsed = array.array('i')
                    parsed.frombytes(payload[:len(payload) - len(payload) % parsed.itemsize])
                    fds.extend(parsed)
            if len(data) > 1024 * 1024:
                raise ValueError('Oversized handshake')
            try:
                mappings = json.loads(data)
            except (json.JSONDecodeError, UnicodeDecodeError):
                continue
            if len(fds) != 1:
                raise ValueError('Expected exactly one UFFD descriptor')
            return mappings, fds.pop()
    finally:
        for fd in fds:
            os.close(fd)


class Handler:
    def __init__(self, args):
        self.args = args
        self.phase = 'restore'
        self.tool_id = None
        self.seq = 0
        self.counts = {}
        self.unique = {}
        self.uffd = None
        self.peer_pid = None
        self.peer_start = None
        self.stop = False
        self.failed = False
        self.memory = os.open(args.memory, os.O_RDONLY)
        self.logfile = open(args.log, 'x', buffering=1)
        os.chmod(args.log, 0o600)
        self.started_ns = time.monotonic_ns()
        self.cpu_start_ns = time.process_time_ns()
        self.log('start', memory_size=os.fstat(self.memory).st_size,
                 semantics='First missing-page fault per cold restore; includes VMM/device/kernel accesses. Flags describe the triggering host fault, not all guest reads/writes.',
                 guest_bases_source=args.guest_bases_source)

    def log(self, event, **fields):
        self.seq += 1
        self.logfile.write(json.dumps({'event': event, 'sequence': self.seq,
            'monotonic_ns': time.monotonic_ns(), 'wall_ns': time.time_ns(),
            'phase': self.phase, 'tool_id': self.tool_id, **fields}, separators=(',', ':'))+'\n')

    def serve(self, msg):
        if len(msg) != 32:
            raise ValueError('Incomplete UFFD event')
        event = msg[0]
        if event == UFFD_EVENT_REMOVE:
            raise ValueError('UFFD REMOVE unsupported: balloon/reclaim must be disabled')
        if event != UFFD_EVENT_PAGEFAULT:
            raise ValueError('Unexpected UFFD event: '+str(event))
        flags, address = struct.unpack_from('=QQ', msg, 8)
        if flags & ~1:
            raise ValueError('Unexpected WP/MINOR/unknown fault mode: '+str(flags))
        mapped = mapping_for(address, self.mappings)
        data = os.pread(self.memory, PAGE, mapped['snapshot_offset'])
        if len(data) != PAGE:
            raise ValueError('Short snapshot read')
        source = ctypes.create_string_buffer(data, PAGE)
        request = bytearray(struct.pack('=QQQQq', mapped['host_page_address'], ctypes.addressof(source), PAGE, 0, 0))
        before = time.monotonic_ns()
        outcome = 'copied'
        try:
            fcntl.ioctl(self.uffd, UFFDIO_COPY, request, True)
        except OSError as exc:
            if exc.errno != errno.EEXIST:
                raise
            outcome = 'already_present'
        copied = struct.unpack_from('=q', request, 32)[0]
        if outcome == 'copied' and copied != PAGE:
            raise ValueError('Incomplete UFFDIO_COPY: '+str(copied))
        key = (self.phase, self.tool_id)
        self.counts[key] = self.counts.get(key, 0) + 1
        self.unique.setdefault(key, set()).add(mapped['snapshot_page'])
        self.log('pagefault', fault_address=address, flags=flags,
                 first_fault_write_flag=bool(flags & 1), result=outcome,
                 copy_ns=time.monotonic_ns()-before, **mapped)

    def control(self, listener):
        conn, _ = listener.accept()
        with conn:
            conn.settimeout(10)
            raw = bytearray()
            while b'\n' not in raw:
                part = conn.recv(4096)
                if not part or len(raw) > 65536:
                    raise ValueError('Invalid control request')
                raw.extend(part)
            request = json.loads(raw.split(b'\n')[0])
            command = request.get('command')
            if command == 'phase':
                name = request.get('phase')
                if not isinstance(name, str) or not name or len(name) > 100:
                    raise ValueError('Invalid phase')
                self.phase = name
                self.tool_id = request.get('tool_id')
                self.log('phase', note='Control acknowledged on handler clock; queued faults may cross this boundary.')
            elif command == 'stop':
                if self.peer_pid and identity(self.peer_pid) == self.peer_start:
                    conn.sendall(b'{"ok":false,"error":"VMM peer still alive; handler must continue serving"}\n')
                    return
                self.stop = True
                self.log('stop_requested')
            elif command != 'status':
                raise ValueError('Unknown control command')
            conn.sendall((json.dumps({'ok':True,'phase':self.phase,'tool_id':self.tool_id,'peer_pid':self.peer_pid,'sequence':self.seq})+'\n').encode())

    def run(self):
        listeners = []
        paths = [self.args.socket, self.args.control]
        try:
            for path in paths:
                listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                listener.bind(path)
                os.chmod(path, 0o600)
                listener.listen(8)
                listeners.append(listener)
            self.log('listening', socket=self.args.socket, control=self.args.control)
            deadline = time.monotonic()+self.args.connect_timeout
            while not self.stop and self.uffd is None:
                ready, _, _ = select.select(listeners, [], [], min(1, max(0, deadline-time.monotonic())))
                if time.monotonic() >= deadline:
                    raise TimeoutError('Timed out awaiting Firecracker handshake')
                if listeners[1] in ready:
                    self.control(listeners[1])
                if listeners[0] in ready:
                    conn, _ = listeners[0].accept()
                    with conn:
                        conn.settimeout(30)
                        self.peer_pid, uid, gid = struct.unpack('=iii', conn.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
                        self.peer_start = identity(self.peer_pid)
                        self.mappings, self.uffd = receive_handshake(conn)
                        os.set_blocking(self.uffd, False)
                        self.mappings = validate_mappings(self.mappings, os.fstat(self.memory).st_size, self.args.guest_bases)
                        self.log('handshake', peer_pid=self.peer_pid, peer_start=self.peer_start, peer_uid=uid, peer_gid=gid, mappings=self.mappings)
            while not self.stop:
                ready, _, _ = select.select([self.uffd, listeners[1]], [], [], 1)
                # Bound each drain so a busy VM cannot starve control boundaries.
                if self.uffd in ready:
                    for _ in range(64):
                        try:
                            msg = os.read(self.uffd, 32)
                        except BlockingIOError:
                            break
                        if not msg:
                            self.stop = True
                            break
                        self.serve(msg)
                if listeners[1] in ready:
                    self.control(listeners[1])
                if identity(self.peer_pid) != self.peer_start:
                    self.log('peer_exited')
                    self.stop = True
        except BaseException as exc:
            self.failed = True
            self.log('failure', error=type(exc).__name__+': '+str(exc))
            raise
        finally:
            self.log('summary', failed=self.failed, elapsed_ns=time.monotonic_ns()-self.started_ns,
                     cpu_ns=time.process_time_ns()-self.cpu_start_ns,
                     phases=[{'phase':p,'tool_id':t,'events':n,'unique_pages':len(self.unique[(p,t)])} for (p,t),n in self.counts.items()])
            self.logfile.close()
            os.close(self.memory)
            if self.uffd is not None:
                os.close(self.uffd)
            for listener in listeners:
                listener.close()
            for path in paths[:len(listeners)]:
                Path(path).unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('socket', 'control', 'memory', 'log'):
        parser.add_argument('--'+name, required=True)
    parser.add_argument('--guest-bases', type=json.loads, help='Optional verified GPA base list in native mapping order')
    parser.add_argument('--guest-bases-source', help='Evidence for supplied GPA bases')
    parser.add_argument('--connect-timeout', type=float, default=120)
    args = parser.parse_args()
    if args.guest_bases is not None and not args.guest_bases_source:
        parser.error('--guest-bases requires --guest-bases-source')
    os.umask(0o077)
    Handler(args).run()


if __name__ == '__main__':
    main()
