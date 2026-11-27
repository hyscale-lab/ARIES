#!/usr/bin/env python3
"""Export and validate the four stock restore-to-ready lineages without reading guest RAM."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import time
import tomllib


def read(path):
    return json.loads(path.read_bytes())


def write(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def config(path):
    data = read(path)
    data["drives"].sort(key=lambda d: d["drive_id"])
    return data


def inventory(meta):
    return [(r["offset"], r["guest_phys_addr"], r["size"], r["page_size"]) for r in meta["regions"]]


def verify(service, out):
    t = service.with_name(service.name + "-tools")
    out.mkdir(mode=0o700)
    shutil.copyfile(t / "effective.toml", out / "effective.toml")
    active = tomllib.loads((out / "effective.toml").read_text())
    assert active["snapshot"]["memory_startup_pack"] == {"enabled": False, "consume_enabled": False}
    assert active["memory_snapshot"]["track_dirty_pages"] is True
    assert active["memory_snapshot"]["background_download"]["enable"] is False
    overlay = read(service / "home/overlaybd/mem-overlaybd-global.json")
    assert overlay["download"]["enable"] is False
    write(out / "memory-device-config.json", {"download": overlay["download"]})
    base = t / "ready/base"
    base_snap = read(base / "catalog.json")
    layer, = base_snap["committed"]["memory_layers"]
    assert not base_snap["committed"]["attached_drives"]
    assert "memory_startup" not in base_snap["committed"]
    boot = (base / "guest.txt").read_text().splitlines()
    assert boot[2:4] == ["6", "Y"] and not any(" /workload/" in line for line in boot)
    topology = config(base / "vm-config.json")
    layout = inventory(read(base / "inspect.json"))
    assert layout == [(0, 0, 3 << 30, 4096), (3 << 30, 4 << 30, 1 << 30, 4096)]
    assert topology["machine-config"]["vcpu_count"] == 2 and topology["machine-config"]["mem_size_mib"] == 4096
    assert topology["machine-config"]["track_dirty_pages"] is True
    assert topology["balloon"]["free_page_reporting"] is True
    assert "page_reporting.page_reporting_order=6" in topology["boot-source"]["boot_args"]
    write(out / "base-vm-config.json", topology)
    shutil.copyfile(base / "inspect.json", out / "base-inspect.json")
    shutil.copyfile(base / "kernel.sha256", out / "kernel.sha256")
    log = [json.loads(line) for line in (service / "server.log").read_text().splitlines()]
    gate = [json.loads(line) for line in (t / "gate.jsonl").read_text().splitlines()]
    dep = service / "home/deps"
    fc_sha = hashlib.sha256((dep / "firecracker/1.15.1-patch-v2/firecracker").read_bytes()).hexdigest()
    srv_sha = hashlib.sha256((service.parent / "server-bundle/server").read_bytes()).hexdigest()
    assert fc_sha == "cad0cd9291e7364dff7270981b277e0094b2d5ab76dbd17e16bbe9e41def9875"
    assert srv_sha == "19b402c82e4ef16c12f2434d579448d933ac943f099c993581226d8b6d8f30ea"
    sources = {}
    for task in ("wal-recovery-ordering", "session-window-debug", "calendar", "chapter-summary"):
        r = t / "ready" / task
        held, = read(r / "held.json")
        setup = read(r / "setup.json")
        cat = read(r / "catalog.json")
        sandbox = (r / "sandbox-id").read_text().strip()
        assert held["pid"] == setup["pid"] and held["state"] == "held"
        assert held["load"]["mem_backend"]["backend_type"] == "File"
        assert held["load"]["resume_vm"] is False and held["load"]["track_dirty_pages"] is True
        assert Path(held["load"]["snapshot_path"]).parent.name == base_snap["id"]
        assert (r / "ancestor-id").read_text().strip() == base_snap["id"]
        assert cat["source"]["Sandbox"]["source_sandbox_id"] == sandbox
        assert cat["resources"] == base_snap["resources"] and len(cat["committed"]["attached_drives"]) == 1
        assert config(r / "vm-config.json") == topology and inventory(setup) == layout
        assert (r / "prepared/boot-id").read_text().strip() == boot[1]
        assert (r / "prepared/guest-setup.txt").read_text().splitlines()[-3:] == ["6", "Y", boot[1]]
        assert read(r / "prepared/workload-state.json")["status"] == "running"
        assert "memory_startup" not in cat["committed"]
        assert "memory-startup.pack" not in (r / "artifacts.txt").read_text().splitlines()
        before, delta = cat["committed"]["memory_layers"]
        assert before == layer
        pid = held["pid"]
        restored = [row for row in log if any(s.get("sandbox_id") == sandbox for s in row.get("spans",[]))]
        starts, = [row for row in restored if row["fields"]["message"] == "starting sandbox from snapshot config"]
        complete, = [row for row in restored if row["fields"]["message"] == "sandbox restored from snapshot config"]
        assert starts["fields"]["vm_state_path"] == held["load"]["snapshot_path"]
        assert not any("prefetch" in row["fields"]["message"].lower() for row in restored)
        events = [e for e in gate if e.get("pid") == pid]
        req, = [e for e in events if e["event"] == "resume_sent"]
        ack, = [e for e in events if e["event"] == "resume_reply"]
        exact, = [e for e in events if e["event"] == "dirty_ranges"]
        assert held["load_done_ns"] < held["held_ns"] < req["send_ns"] < ack["reply_ns"] < exact["reply_ns"]
        assert setup["dirty"] == read(t / exact["archive"])
        assert sum(row["length"] for row in setup["dirty"]["ranges"]) == setup["setup_dirty_bytes"]
        shutil.copyfile(r / "setup.json", out / (task+"-setup.json"))
        write(out / (task+"-gate.json"), events)
        sources[task] = dict(ready_snapshot=cat["id"], sandbox_id=sandbox, pid=pid,
                             image_offset_layout_matches_base=True, stock_vm_config_matches_base=True,
                             boot_id_matches_base=True, container_running=True, ancestor_layer=before,
                             setup_layer=delta, setup_dirty_bytes=setup["setup_dirty_bytes"],
                             startup_manifest_in_catalog=False, startup_manifest_in_artifact_dir=False,
                             prefetch_log_count=0, native_dirty_reply_ns=exact["reply_ns"],
                             restore_start=starts["timestamp"], restore_ready=complete["timestamp"])
    write(out / "lineage.json", dict(host=socket.getfqdn(), base_snapshot=base_snap["id"],
          base_sandbox=base_snap["source"]["Sandbox"]["source_sandbox_id"], base_memory_layer=layer,
          kernel=boot[0], boot_id=boot[1], firecracker_sha256=fc_sha, agentenv_server_sha256=srv_sha,
          startup_pack_enabled=False, startup_pack_consume_enabled=False,
          local_prefetch_issued=False, runner_guest_memory_preread=False,
          release="v0.2.5", sources=sources))
    hardware = {"host":socket.getfqdn(), "monotonic_ns":time.monotonic_ns(),
                "online":Path("/sys/devices/system/cpu/online").read_text().strip(),
                "smt_active":int(Path("/sys/devices/system/cpu/smt/active").read_text()),"cores":[]}
    for cpu in sorted(Path("/dev/cpu").glob("[0-9]*"),key=lambda p:int(p.name)):
        if not (cpu / "msr").exists():
            continue
        with (cpu / "msr").open("rb",buffering=0) as f:
            def msr(address):
                return int.from_bytes(os.pread(f.fileno(), 8, address), "little")
            p0 = msr(0xc0010064)
            hardware["cores"].append(dict(cpu=int(cpu.name),p0_mhz=200*(p0 & 255)/((p0 >> 8) & 63),
                pstate_requested=msr(0xc0010062)&7, boost_disabled=bool(msr(0xc0010015)&(1<<25))))
    write(out / "host.json", hardware)


if __name__ == "__main__":
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("service", type=Path)
    p.add_argument("out", type=Path)
    args=p.parse_args()
    verify(args.service,args.out)
