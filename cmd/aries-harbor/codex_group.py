"""Linux guest helper for the Codex 0.151.0 local pipe process-group lifecycle."""

import ctypes
import os
import selectors
import signal
import socket
import sys
import threading
import time


def copy_output(source, destination):
    data = os.read(source, 65536)
    if not data:
        return False
    view = memoryview(data)
    while view:
        view = view[os.write(destination, view):]
    return True


def supervise(listener, status, pipes, command, interruptible):
    # Reap orphaned group members so a later kill -0 sees their true lifetime.
    if ctypes.CDLL(None, use_errno=True).prctl(36, 1, 0, 0, 0) != 0:
        raise OSError(ctypes.get_errno(), "PR_SET_CHILD_SUBREAPER")
    if interruptible:
        ready_read, ready_write = os.pipe()
    child = os.fork()
    if not child:
        os.setsid()
        os.dup2(pipes[0][1], 1)
        os.dup2(pipes[1][1], 2)
        for sig in (signal.SIGPIPE, signal.SIGXFSZ):
            signal.signal(sig, signal.SIG_DFL)
        if interruptible:
            signal.signal(signal.SIGINT, signal.SIG_DFL)
            os.close(ready_read)
            os.write(ready_write, b'1')
            os.close(ready_write)
        try:
            os.execv(command[0], command)
        except OSError as error:
            print(error, file=sys.stderr, flush=True)
            os._exit(127 if isinstance(error, FileNotFoundError) else 126)
    if interruptible:
        os.close(ready_write)
        with os.fdopen(ready_read, 'rb', buffering=0) as ready:
            if ready.read(1) != b'1':
                raise RuntimeError("interruptible process group did not start")
    for pipe in pipes:
        for fd in pipe:
            os.close(fd)
    with open(os.devnull, "r+b", buffering=0) as null:
        for fd in (0, 1, 2):
            os.dup2(null.fileno(), fd)
    # WNOWAIT retains the real group leader and prevents PID reuse while the
    # host has a yielded handle. Only its matching release can clean that group.
    def report_exit():
        with status:
            done = os.waitid(os.P_PID, child, os.WEXITED | os.WNOWAIT)
            code = done.si_status if done.si_code == os.CLD_EXITED else 128 + done.si_status
            try:
                status.sendall(bytes([code]))
            except BrokenPipeError:
                pass

    if interruptible:
        waiter = threading.Thread(target=report_exit)
        waiter.start()
    else:
        report_exit()
    while True:
        conn, _ = listener.accept()
        with conn:
            request = conn.recv(1)
            if request == b'I' and interruptible:
                try:
                    os.killpg(child, signal.SIGINT)
                except OSError:
                    acknowledgement = b'E'
                else:
                    acknowledgement = b'S'
                try:
                    conn.sendall(acknowledgement)
                except BrokenPipeError:
                    pass
                continue
            if not request:
                continue
            if request != b'R':
                raise RuntimeError("invalid release request")
            os.killpg(child, signal.SIGKILL)
            if interruptible:
                waiter.join()
            os.waitpid(child, 0)
            while True:
                try:
                    os.waitpid(-child, 0)
                except ChildProcessError:
                    break
            conn.sendall(b'K')
            return


def run(address, command, interruptible):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as listener:
        listener.bind(address)
        listener.listen(1)
        status, child_status = socket.socketpair()
        pipes = [os.pipe(), os.pipe()]
        if os.fork() == 0:
            status.close()
            try:
                supervise(listener, child_status, pipes, command, interruptible)
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
    if mode in ('start', 'start-interruptible') and command:
        return run(address, command, mode == 'start-interruptible')
    if mode not in ('release', 'interrupt') or command:
        raise ValueError("expected start with argv, release, or interrupt")
    request, acknowledgement = (b'R', b'K') if mode == 'release' else (b'I', b'S')
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.connect(address)
        conn.sendall(request)
        if conn.recv(1) != acknowledgement:
            raise RuntimeError(f"guest did not acknowledge group {mode}")
    return 0


if __name__ == '__main__':
    sys.exit(main())
