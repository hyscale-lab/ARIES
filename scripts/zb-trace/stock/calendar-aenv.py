#!/usr/bin/env python3
"""Run a WildClaw fixture's cold API request with time for OCI conversion."""
import json
import os
from pathlib import Path
import sys
import tomllib
from urllib.request import Request, urlopen


args = sys.argv[1:]
if args[:2] != ["start", "--cold"]:
    cli = os.environ["STOCK_AENV_CLI"]
    os.execv(cli, [cli, *args])

expected = ["start", "--cold", "-d", "--timeout", "3600", "--cpu"]
assert len(args) == 12 and args[:6] == expected and args[7] == "--memory" and (args[6],args[8]) in {("1","512"),("1","2048"),("10","8192")} and args[9] == "--image-mount"
mount, image = args[10].split("=", 1)
assert mount == "/workload/rootfs"
with (Path(os.environ["XDG_CONFIG_HOME"]) / "aenv/credentials").open("rb") as source:
    credentials = tomllib.load(source)
assert credentials["url"] == "http://127.0.0.1:18763"
body = dict(image=args[11], timeout=3600, cpuCount=int(args[6]), memoryMB=int(args[8]),
            secure=True, attachedDrives=[dict(driveID="image_workload_rootfs", readOnly=False,
                                              mountPath=mount, source=dict(image=image))])
request = Request(credentials["url"] + "/sandboxes-cold", json.dumps(body).encode(),
                  {"X-API-Key": credentials["api_key"], "Content-Type": "application/json"})
with urlopen(request, timeout=900) as response:
    assert response.status == 201
    print(json.load(response)["sandboxID"])
