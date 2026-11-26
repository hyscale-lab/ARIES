"""Expose appliance requests to the ARIES calendar backend over private pipes."""
import json
import signal
import sys

from host import Appliance


def interrupt(signum, frame):
    raise KeyboardInterrupt


def main():
    signal.signal(signal.SIGTERM, interrupt)
    signal.signal(signal.SIGHUP, interrupt)
    appliance = Appliance(stream=sys.stderr)
    try:
        for line in sys.stdin:
            request = json.loads(line)
            op = request.pop("op")
            try:
                if op == "ready":
                    finished, result = appliance.receive(120)
                    response = dict(finished_ns=finished, result=result)
                elif op == "memory":
                    finished = appliance.memory(request["phase"])
                    response = dict(finished_ns=finished)
                elif op == "event":
                    finished = appliance.emit(request["event"], **(request.get("fields") or {}))
                    response = dict(finished_ns=finished)
                else:
                    started, finished, result = appliance.request(op, **request)
                    response = dict(started_ns=started, finished_ns=finished, result=result)
                print(json.dumps(response), flush=True)
                if op == "shutdown":
                    return
            except Exception as exc:
                appliance.emit("first_failure", operation=op, error=repr(exc))
                print(json.dumps({"error": repr(exc)}), flush=True)
                raise
    finally:
        appliance.close()


if __name__ == "__main__":
    main()
