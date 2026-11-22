"""Phase 0 adapters for API-driven sandboxes: CubeSandbox (E2B API, KVM
microVMs) and OpenSandbox (lifecycle server + execd, Docker runtime).
Imported by probe.py when present."""

import json
import os
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


class Result:
    def __init__(self, stdout, stderr, code):
        self.stdout, self.stderr, self.code = stdout, stderr, code


# --- CubeSandbox -----------------------------------------------------------

class Cube:
    """Exact argv through envd's own process start, as the E2B SDK's
    commands.run does with a shell; stdin through its input stream."""

    name = "cube"

    def __init__(self):
        from e2b import Sandbox  # noqa: PLC0415
        from e2b.sandbox_sync.commands import command as module  # noqa: PLC0415
        self.Sandbox, self.module = Sandbox, module
        self.template = os.environ.get("CUBE_TEMPLATE_ID", "probe-py")
        self.api = os.environ.get("E2B_API_URL", "http://127.0.0.1:3000")
        self.key = os.environ.get("E2B_API_KEY", "e2b_000000")
        self.boxes = {}

    def create(self):
        box = self.Sandbox.create(template=self.template, timeout=3600)
        self.boxes[box.sandbox_id] = box
        return box.sandbox_id

    def _start(self, box, argv, stdin):
        m, commands = self.module, box.commands
        events = m.as_stream(commands._rpc.start(
            m.process_pb.StartRequest(process=m.process_pb.ProcessConfig(cmd=argv[0], args=list(argv[1:])),
                                      stdin=stdin),
            headers={**m.authentication_header(commands._envd_version, None),
                     m.KEEPALIVE_PING_HEADER: str(m.KEEPALIVE_PING_INTERVAL_SEC)}))
        return commands._create_handle(events, "start process", True)

    def exec(self, sandbox, argv, stdin=None, timeout=300):
        box = self.boxes[sandbox]
        handle = self._start(box, argv, stdin is not None)
        if stdin is not None:
            # The E2B input API takes text; binary stdin is a finding.
            box.commands.send_stdin(handle.pid, stdin.decode("utf-8", errors="surrogateescape"))
            box.commands.close_stdin(handle.pid)
        try:
            r = handle.wait()
            return Result(r.stdout.encode(), r.stderr.encode(), r.exit_code)
        except Exception as error:  # CommandExitException carries the result
            code = getattr(error, "exit_code", None)
            if code is None:
                raise
            return Result((error.stdout or "").encode(), (error.stderr or "").encode(), code)

    def exec_cancel(self, sandbox, argv, after):
        handle = self._start(self.boxes[sandbox], argv, False)
        time.sleep(after)
        handle.disconnect()  # closes the transport, like ARIES's cancellation

    def upload(self, sandbox, local, remote):
        with open(local, "rb") as handle:
            self.boxes[sandbox].files.write(remote, handle.read())

    def download(self, sandbox, remote, local):
        try:
            data = self.boxes[sandbox].files.read(remote, format="bytes")
        except Exception as error:
            return False, "%s: %s" % (type(error).__name__, error)
        with open(local, "wb") as handle:
            handle.write(bytes(data))
        return True, ""

    def delete(self, sandbox):
        self.boxes[sandbox].kill()

    def absent(self, sandbox):
        request = urllib.request.Request("%s/sandboxes/%s" % (self.api, sandbox), headers={"X-API-Key": self.key})
        try:
            urllib.request.urlopen(request, timeout=10).read()
            return False
        except urllib.error.HTTPError as error:
            return error.code == 404


# --- OpenSandbox -----------------------------------------------------------

class OpenSandbox:
    """Lifecycle REST API, then execd directly at the endpoint the server
    resolves. execd's one-off commands take no stdin."""

    name = "opensandbox"

    def __init__(self):
        self.api = os.environ.get("OPENSANDBOX_URL", "http://127.0.0.1:8080")
        with open(os.path.expanduser("~/.opensandbox-key")) as handle:
            self.key = handle.read().strip()
        self.endpoints = {}
        self.lock = threading.Lock()

    def _call(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(self.api + path, data=data, method=method,
                                         headers={"OPEN-SANDBOX-API-KEY": self.key, "Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=300) as response:
            text = response.read()
            return response.status, (json.loads(text) if text else None)

    def create(self):
        _, body = self._call("POST", "/v1/sandboxes", {
            "image": {"uri": "python:3.12.12-slim-bookworm"}, "entrypoint": ["sleep", "infinity"],
            "resourceLimits": {"cpu": "1", "memory": "1Gi"}, "timeout": 3600,
            "metadata": {"probe": uuid.uuid4().hex[:8]}})
        sandbox = body["id"]
        deadline = time.time() + 300
        while body.get("status", {}).get("state") != "Running":
            if time.time() > deadline:
                raise RuntimeError("not running: %s" % body)
            time.sleep(0.05)
            _, body = self._call("GET", "/v1/sandboxes/" + sandbox)
        _, endpoint = self._call("GET", "/v1/sandboxes/%s/endpoints/44772" % sandbox)
        self.endpoints[sandbox] = "http://" + endpoint["endpoint"]
        # Running is reported before execd listens; wait for its /ping.
        while True:
            try:
                urllib.request.urlopen(self.endpoints[sandbox] + "/ping", timeout=5).read()
                break
            except (urllib.error.URLError, ConnectionError):
                if time.time() > deadline:
                    raise
                time.sleep(0.02)
        return sandbox

    def _events(self, sandbox, body):
        request = urllib.request.Request(self.endpoints[sandbox] + "/command", data=json.dumps(body).encode(),
                                         method="POST", headers={"Content-Type": "application/json"})
        response = urllib.request.urlopen(request, timeout=600)
        return response

    @staticmethod
    def _parse(line):
        line = line.strip()
        if line.startswith(b"data:"):
            line = line[5:].strip()
        return json.loads(line) if line.startswith(b"{") else None

    def exec(self, sandbox, argv, stdin=None, timeout=300):
        if stdin is not None:
            return Result(b"", b"stdin unsupported by execd /command", None)
        stdout, stderr, code = [], [], None
        with self._events(sandbox, {"argv": list(argv)}) as response:
            for line in response:
                event = self._parse(line)
                if not event:
                    continue
                kind = event.get("type")
                if kind == "stdout":
                    stdout.append(event.get("text", ""))
                elif kind == "stderr":
                    stderr.append(event.get("text", ""))
                elif kind == "execution_complete":
                    code = event.get("exit_code", 0)
                elif kind == "error":
                    code = (event.get("error") or {}).get("exit_code")
        return Result("".join(stdout).encode(), "".join(stderr).encode(), code)

    def exec_cancel(self, sandbox, argv, after):
        response = self._events(sandbox, {"argv": list(argv)})
        time.sleep(after)
        response.close()  # closes the transport, like ARIES's cancellation

    def upload(self, sandbox, local, remote):
        boundary = uuid.uuid4().hex
        with open(local, "rb") as handle:
            data = handle.read()
        body = b"".join([
            b"--%s\r\nContent-Disposition: form-data; name=\"metadata\"; filename=\"metadata\"\r\nContent-Type: application/json\r\n\r\n"
            % boundary.encode(), json.dumps({"path": remote}).encode(), b"\r\n",
            b"--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"f\"\r\n"
            b"Content-Type: application/octet-stream\r\n\r\n" % boundary.encode(), data, b"\r\n",
            b"--%s--\r\n" % boundary.encode()])
        request = urllib.request.Request(self.endpoints[sandbox] + "/files/upload", data=body, method="POST",
                                         headers={"Content-Type": "multipart/form-data; boundary=" + boundary})
        urllib.request.urlopen(request, timeout=300).read()

    def download(self, sandbox, remote, local):
        url = self.endpoints[sandbox] + "/files/download?" + urllib.parse.urlencode({"path": remote})
        try:
            data = urllib.request.urlopen(url, timeout=300).read()
        except urllib.error.HTTPError as error:
            return False, "HTTP %d %s" % (error.code, error.read()[:100])
        with open(local, "wb") as handle:
            handle.write(data)
        return True, ""

    def delete(self, sandbox):
        self._call("DELETE", "/v1/sandboxes/" + sandbox)

    def absent(self, sandbox):
        try:
            _, body = self._call("GET", "/v1/sandboxes/" + sandbox)
        except urllib.error.HTTPError as error:
            api_gone = error.code == 404
        else:
            api_gone = False
        import subprocess  # noqa: PLC0415
        left = subprocess.run(["docker", "ps", "-aq", "--filter", "label=opensandbox.io/id=" + sandbox],
                              capture_output=True, text=True).stdout.strip()
        return api_gone and not left


BACKENDS = {"cube": Cube, "opensandbox": OpenSandbox}
