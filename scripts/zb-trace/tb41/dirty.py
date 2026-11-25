#!/usr/bin/env python3
"""Read and reset the native Firecracker KVM bitmap at 1 Hz."""
import argparse
import base64
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import signal
import socket
import struct
import sys
import time

SECOND = 10**9
ACTION = b'{"action_type":"ZepboundSampleDirty"}'
BITMAP_ACTION = b'{"action_type":"ZepboundSampleDirtyBitmap"}'


def read_log(api, start_identity, bitmap=False):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
        sock.settimeout(5)
        dirfd = os.open(api.path.parent, os.O_DIRECTORY)
        try:
            sock.connect(f"/proc/self/fd/{dirfd}/{api.path.name}")
        finally:
            os.close(dirfd)
        pid, _, _ = struct.unpack("3i", sock.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        if (pid, start_identity(pid)) != api.identity:
            raise RuntimeError("Firecracker API peer changed")
        conn = http.client.HTTPConnection("localhost")
        conn.sock = sock
        conn.request("PUT", "/actions", BITMAP_ACTION if bitmap else ACTION,
                     {"Content-Type": "application/json"})
        response = conn.getresponse()
        payload = response.read()
        if response.status != 200:
            raise RuntimeError(f"Firecracker dirty log HTTP {response.status}: {payload.decode()}")
        return json.loads(payload)


def validate(sample, page_size, expected):
    slots = sample["slots"]
    if any(type(row[key]) is not int for row in slots
           for key in ("slot", "guest_base", "pages", "dirty_pages")):
        raise RuntimeError("noninteger Firecracker dirty slot fields")
    actual = {row["slot"]: (row["guest_base"], row["pages"] * page_size) for row in slots}
    if (type(sample["page_size"]) is not int or sample["page_size"] != page_size
            or len(actual) != len(slots) or actual != expected
            or any(not 0 <= row["dirty_pages"] <= row["pages"] for row in slots)):
        raise RuntimeError("Firecracker dirty bitmap disagrees with registered guest RAM")
    return sum(row["dirty_pages"] for row in slots)


def decode_bitmaps(sample, previous_epoch=None):
    epoch = sample["epoch"]
    if type(epoch) is not int or epoch < 1 or (previous_epoch is not None and epoch != previous_epoch + 1):
        raise RuntimeError(f"discontinuous Firecracker dirty log epoch: {previous_epoch} -> {epoch}")
    bitmaps = {}
    cursor = sample["started_us"]
    finish = cursor + sample["elapsed_us"]
    for row in sample["slots"]:
        pages = row["pages"]
        bitmap = base64.b64decode(row["bitmap_b64"], validate=True)
        tail_bits = pages % 8
        if (pages <= 0 or len(bitmap) != (pages + 7) // 8
                or (tail_bits and bitmap[-1] >> tail_bits)
                or int.from_bytes(bitmap, "little").bit_count() != row["dirty_pages"]):
            raise RuntimeError(f"invalid Firecracker bitmap for slot {row['slot']}")
        if not cursor <= row["read_start_us"] <= row["read_finish_us"] <= finish:
            raise RuntimeError(f"invalid Firecracker bitmap timing for slot {row['slot']}")
        cursor = row["read_finish_us"]
        bitmaps[row["slot"]] = bitmap
    return bitmaps


def write_bitmaps(sample, bitmap_dir, index, previous_epoch=None):
    bitmaps = decode_bitmaps(sample, previous_epoch)
    for row in sample["slots"]:
        bitmap = bitmaps[row["slot"]]
        name = f"{index:08d}-slot-{row['slot']}.bin"
        with (bitmap_dir / name).open("xb") as output:
            output.write(bitmap)
        del row["bitmap_b64"]
        row.update(bitmap_file=name, bitmap_bytes=len(bitmap),
                   bitmap_sha256=hashlib.sha256(bitmap).hexdigest())
    return sample["epoch"]


