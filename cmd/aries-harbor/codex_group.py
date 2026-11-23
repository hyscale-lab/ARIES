"""Linux guest helper for the Codex 0.151.0 local pipe process-group lifecycle."""

import ctypes
import os
import selectors
import signal
import socket
import sys
import time


def copy_output(source, destination):
    data = os.read(source, 65536)
    if not data:
        return False
    view = memoryview(data)
    while view:
        view = view[os.write(destination, view):]
    return True


def supervise(listener, status, pipes, command):
    # Reap orphaned group members so a later kill -0 sees their true lifetime.
    if ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "PR_SET_CHILD_SUBREAPER")
    child = os.fork()
    if not child:
        os.setsid()
        os.dup2(pipes[0][1], 1)
        os.dup2(pipes[1][1], 2)
        for sig in (signal.SIGPIPE, signal.SIGXFSZ):
            signal.signal(sig, signal.SIG_DFL)
        try:
            os.execv(command[0], command)
        except OSError as error:
            print(error, file=sys.stderr, flush=True)
            os._exit(127 if isinstance(error, FileNotFoundError) else 126)
    for pipe in pipes:
        for fd in pipe:
            os.close(fd)
    with open(os.devnull, "r+b", buffering=0) as null:
        for fd in (0, 1, 2):
            os.dup2(null.fileno(), fd)
    # WNOWAIT retains the real group leader and prevents PID reuse while the
    # host has a yielded handle. Only its matching release can clean that group.
    done = os.waitid(os.P_PID, child, os.WEXITED | os.WNOWAIT)
    code = done.si_status if done.si_code == os.CLD_EXITED else 128 + done.si_status
    status.sendall(bytes([code]))
    status.close()
    conn, _ = listener.accept()
    with conn:
        if conn.recv(1) != b'R':
            raise RuntimeError("invalid release request")
        os.killpg(child, signal.SIGKILL)
        os.waitpid(child, 0)
        while True:
            try:
                os.waitpid(-child, 0)
            except ChildProcessError:
                break
        conn.sendall(b'K')


def run(address, command):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
        listener.bind(address)
        listener.listen(1)
        status, child_status = socket.socketpair()
        pipes = [os.pipe(), os.pipe()]
        if os.fork() == 0:
            status.close()
            try:
                supervise(listener, child_status, pipes, command)
            finally:
                os._exit(0)
        child_status.close()
    with status, selectors.DefaultSelector() as selector:
        selector.register(status, selectors.EVENT_READ, 0)
        for dest, (read, write) in enumerate(pipes, 1):
            os.close(write)
            selector.register(read, selectors.EVENT_READ, dest)
        code, deadline = None, None
        while selector.get_map():
            timeout = max(0, deadline - time.monotonic()) if deadline is not None else None
            if timeout == 0:
                break
            for key, _ in selector.select(timeout):
                if key.data == 0:
                    result = status.recv(1)
                    if not result:
                        raise RuntimeError("guest process supervisor exited without a shell status")
                    code = result[0]
                    deadline = time.monotonic() + 0.050
                    selector.unregister(status)
                elif not copy_output(key.fd, key.data):
                    selector.unregister(key.fd)
                    os.close(key.fd)
        return code


def main():
    mode, token, *command = sys.argv[1:]
    address = b'\x00aries-codex-' + token.encode('ascii')
    if mode == 'start' and command:
        return run(address, command)
    if mode != 'release' or command:
        raise ValueError("expected start with argv or release")
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.connect(address)
        conn.sendall(b'R')
        if conn.recv(1) != b'K':
            raise RuntimeError("guest did not acknowledge group release")
    return 0


if __name__ == '__main__':
    sys.exit(main())
