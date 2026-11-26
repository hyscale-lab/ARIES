#!/usr/bin/env python3
"""Stock POSIX checkpoint lifecycle. stdout is a private pipe to the replay process."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import select
import sys
import time
import tomllib
import urllib.request
from uuid import UUID

from host_sample import bind, process_status
from pack import PAGE, pages

now = time.monotonic_ns


def uid(value):
    if str(UUID(value)) != value:
        raise ValueError("expected a canonical stock UUID")
    return value


def atomic(path, obj):
    temp = path.with_name(path.name + ".tmp")
    temp.write_text(json.dumps(obj) + "\n")
    temp.replace(path)


def rows_after(log, start, end=None):
    with log.open('rb') as f:
        f.seek(start)
        while end is None or f.tell() < end:
            line = f.readline()
            if not line.endswith(b'\n'):
                break
            yield json.loads(line)


def workload_socket(rows, sandbox):
    dirs = {row['fields']['fc_cwd'] for row in rows
            if row['fields'].get('message') == 'starting sandbox from snapshot config'
            and any(s.get('sandbox_id') == sandbox for s in [*row.get('spans', []), row.get('span', {})])}
    if len(dirs) != 1:
        raise ValueError(f'expected one workload socket for {sandbox}, got {len(dirs)}')
    return str(Path(dirs.pop()) / 'firecracker.socket')


def recorder_socket(rows, sandbox):
    dirs = {Path(r['fields']['dir']) for r in rows if r['fields'].get('message') == 'startup pack recording started'
            and Path(r['fields']['dir']).parent.name == sandbox}
    sockets = {str(Path(r['fields']['fc_cwd']) / 'firecracker.socket') for r in rows
               if r['fields'].get('message') == 'starting sandbox from snapshot config'
               and Path(r['fields']['vm_state_path']).parent in dirs}
    if len(sockets) > 1 or len(dirs) > 1:
        raise ValueError('ambiguous startup recorder VM')
    return next(iter(sockets), None)


def managed_layers(record):
    c = record['committed']
    refs = list(c['rootfs_layers'])
    for drive in c['attached_drives']:
        refs.extend(drive['Overlaybd']['layers'])
    for volume in c['volume_snapshots']:
        refs.extend(volume['layers'])
    layers = list(c['memory_layers'])
    for ref in refs:
        if set(ref) == {'Managed'}:
            layers.append(ref['Managed'])
        elif set(ref) != {'External'}:
            raise ValueError('unsupported stock disk layer')
    result = {}
    for layer in layers:
        key, size = layer['digest'], layer['size']
        if not re.fullmatch(r'sha256:[a-f0-9]{64}', key) or type(size) is not int or size < 0:
            raise ValueError('invalid managed layer descriptor')
        if result.setdefault(key, size) != size:
            raise ValueError('inconsistent managed layer sizes')
    return result


def differential(store, before, parent, child):
    old, layers = managed_layers(parent), managed_layers(child)
    delta = {}
    for digest, size in layers.items():
        name = digest.replace(':', '_') + '.overlaybd.commit'
        if digest in old or name in before:
            continue
        if (store/'managed-layers'/name).stat().st_size != size:
            raise ValueError('managed layer size differs from committed descriptor')
        delta[digest] = size
    artifacts = store/'snapshots'/child['id']
    metadata = {n: (artifacts/n).stat().st_size for n in ('vm_state.bin', 'firecracker-manifest.json')}
    return dict(new_managed_layers=delta, differential_layer_bytes=sum(delta.values()), snapshot_artifacts=metadata,
                differential_snapshot_bytes=sum(delta.values()) + sum(metadata.values()))


def prefetch(rows, sandbox, planned):
    # The drain itself has no sandbox ID in v0.2.5. Require an exclusive launch in the bounded log slice.
    starts = [row for row in rows if row['fields'].get('message') == 'creating sandbox']
    drained = [row for row in rows if row['fields'].get('message') == 'memory device startup prefetch drained']
    if (len(starts) != 1 or starts[0].get('span', {}).get('sandbox_id') != sandbox
            or any(r['fields'].get('message') == 'resuming sandbox' for r in rows)
            or len(drained) != 1 or drained[0]['timestamp'] < starts[0]['timestamp']):
        raise ValueError('ambiguous or absent stock local prefetch completion')
    fields = drained[0]['fields']
    if fields['requested_bytes'] != planned or not 0 <= fields['read_bytes'] <= planned:
        raise ValueError('stock prefetch plan differs from the retained manifest')
    return dict(requested_bytes=planned, read_bytes=fields['read_bytes'], native_timestamp=drained[0]['timestamp'])


class Pilot:
    def __init__(self, a):
        self.a = a
        self.store = a.service/'home/snapshot-store/repository'
        self.log = a.service/'server.log'
        self.bindings = a.output/'bindings.json'
        self.events = a.output/'lifecycle.jsonl'
        self.credentials = tomllib.loads((a.service/'xdg/aenv/credentials').read_text())
        if self.credentials['url'].rstrip('/') != a.url:
            raise ValueError('service URL differs from replay URL')
        self.roles = json.loads(self.bindings.read_text())
        self.work = self.roles['workload']

    def event(self, event, **fields):
        with self.events.open('a') as f:
            f.write(json.dumps(dict(event=event, step=self.a.step, phase=self.a.phase, time_ns=now(), **fields)) + '\n')

    def api(self, method, path, body=None):
        request = urllib.request.Request(self.a.url + path, method=method,
                data=json.dumps(body).encode() if body is not None else None,
                headers={'X-API-Key': self.credentials['api_key'], 'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=240) as response:
            raw = response.read()
            return json.loads(raw) if raw else {}

    def detail(self, sandbox, snapshot):
        row = self.api('GET', '/sandboxes/' + sandbox)
        if (row['sandboxID'] != sandbox or row['templateID'] != snapshot or row['state'].lower() != 'running'
                or not row.get('envdAccessToken') or row.get('lifecycle', {}).get('autoResume')):
            raise ValueError('unexpected sandbox identity, state, automatic resume or guest authorization')
        return row

    def record(self, snapshot):
        row = json.loads((self.store/'catalog/records'/f'{snapshot}.json').read_bytes())
        if row['id'] != snapshot or not row['committed'] or row.get('deleting'):
            raise ValueError('invalid committed snapshot')
        return row

    def checkpoint(self):
        a = self.a
        self.detail(a.sandbox, a.snapshot)
        parent = self.record(a.snapshot)
        start_offset = self.log.stat().st_size
        before = set(os.listdir(self.store/'managed-layers'))
        source_socket = Path(self.work['socket'])
        if self.work.get('sandbox_id', a.sandbox) != a.sandbox:
            raise ValueError('binding is for another source sandbox')
        proc, fd, pid, ticks = bind(source_socket)
        try:
            pidfd = os.pidfd_open(pid)
            try:
                if process_status(proc)[1] != ticks:
                    raise ValueError('source process changed before checkpoint')
                self.roles['recording'] = dict(generation=self.work['generation'], socket=None, allow_zero=False)
                atomic(self.bindings, self.roles)
                start = now()
                snapshot = uid(self.api('POST', '/sandboxes/' + a.sandbox + '/snapshots', {})['snapshotID'])
                self.event('published', source_sandbox_id=a.sandbox, parent_snapshot_id=a.snapshot, snapshot_id=snapshot,
                           source_pid=pid, source_start_ticks=ticks, begin_ns=start)
                child = self.record(snapshot)
                if child['source']['Sandbox']['source_sandbox_id'] != a.sandbox:
                    raise ValueError('snapshot did not come from the bound sandbox')
                recorder = recorder_socket(list(rows_after(self.log, start_offset)), a.sandbox)
                if recorder:
                    self.roles['recording']['socket'] = recorder
                    atomic(self.bindings, self.roles)
                self.api('DELETE', '/sandboxes/' + a.sandbox)
                if not select.select([pidfd], [], [], 15)[0]:
                    raise ValueError('source VMM still alive after delete')
            finally:
                os.close(pidfd)
        finally:
            os.close(fd)
        self.work['allow_zero'] = True
        atomic(self.bindings, self.roles)
        self.event('source_exit', snapshot_id=snapshot, source_sandbox_id=a.sandbox, source_pid=pid, source_start_ticks=ticks)
        print(json.dumps({'suspended': True}), flush=True)
        deadline = time.monotonic() + 90
        while True:
            child = self.record(snapshot)
            desc = child['committed'].get('memory_startup')
            if desc:
                break
            if time.monotonic() >= deadline:
                raise TimeoutError('published snapshot has no committed startup descriptor')
            time.sleep(.05)
        logs = list(rows_after(self.log, start_offset))
        if not recorder:
            recorder = recorder_socket(logs, a.sandbox)
        self.roles['recording'].update(socket=recorder, allow_zero=bool(recorder))
        atomic(self.bindings, self.roles)
        built = [r for r in logs if r['fields'].get('message') == 'startup manifest built'
                 and r['fields'].get('snapshot_id') == snapshot]
        blob = (self.store/'snapshots'/snapshot/'memory-startup.pack').read_bytes()
        digest = hashlib.sha256(blob).hexdigest()
        if (len(built) != 1 or built[0]['fields']['manifest_bytes'] != len(blob) or len(blob) != desc['pack_size']
                or digest != desc['index_sha256'] or int.from_bytes(blob[16:24], 'little') != desc['mem_virtual_size']):
            raise ValueError('descriptor, native build log and manifest disagree')
        # Read the server's actual config, including any non-default prefetch budget.
        server = int((a.service/'server.pid').read_text())
        args = Path(f'/proc/{server}/cmdline').read_bytes().decode().split('\0')
        config = tomllib.loads(Path(args[args.index('--config') + 1]).read_text())
        prefs = config['snapshot']['memory_startup_pack']
        if not prefs['enabled'] or not prefs['consume_enabled']:
            raise ValueError('pack recording and local consumption must both be enabled')
        budget = prefs.get('max_pack_bytes', 1 << 30)
        artifact = f'pack-{snapshot}.bin'
        (a.output/artifact).write_bytes(blob)
        self.event('pack_built', source_sandbox_id=a.sandbox, parent_snapshot_id=a.snapshot,
                   snapshot_id=snapshot, manifest=artifact, descriptor=desc, budget_bytes=budget,
                   build_timestamp=built[0]['timestamp'], source_generation=self.work['generation'],
                   recorder_socket=recorder, **differential(self.store, before, parent, child))
        return dict(snapshot=snapshot)

    def restore(self):
        a = self.a
        offset = self.log.stat().st_size
        self.work.update(generation=self.work['generation'] + 1, socket=None, allow_zero=False, sandbox_id=None)
        atomic(self.bindings, self.roles)
        start = now()
        reply = self.api('POST', '/v2/sandboxes', {'templateID': a.snapshot, 'timeout': 3600, 'autoResume': {'enabled': False}})
        sandbox = uid(reply['sandboxID'])
        self.event('restore_created', snapshot_id=a.snapshot, sandbox_id=sandbox, begin_ns=start, log_start=offset)
        detail = self.detail(sandbox, a.snapshot)
        path = workload_socket(rows_after(self.log, offset), sandbox)
        _, fd, pid, ticks = bind(Path(path))
        os.close(fd)
        self.work.update(socket=path, sandbox_id=sandbox)
        atomic(self.bindings, self.roles)
        self.event('restore_ready', snapshot_id=a.snapshot, sandbox_id=sandbox, pid=pid,
                   start_ticks=ticks, generation=self.work['generation'], socket=path)
        return dict(snapshot=a.snapshot, sandbox=sandbox, token=detail['envdAccessToken'])

    def audit(self):
        events = [json.loads(line) for line in self.events.read_text().splitlines()]
        builds = {e['snapshot_id']: e for e in events if e['event'] == 'pack_built'}
        restores = [e for e in events if e['event'] == 'restore_created']
        for i, e in enumerate(restores):
            b = builds[e['snapshot_id']]
            blob = (self.a.output/b['manifest']).read_bytes()
            planned = sum(PAGE for _ in pages(blob, b['budget_bytes']))
            end = restores[i+1]['log_start'] if i+1 < len(restores) else None
            deadline = time.monotonic() + 30
            while True:
                logs = list(rows_after(self.log, e['log_start'], end))
                if end is not None or any(r['fields'].get('message') == 'memory device startup prefetch drained' for r in logs):
                    break
                if time.monotonic() > deadline:
                    raise TimeoutError('no stock prefetch completion for the last restore')
                time.sleep(.05)
            self.event('prefetch', snapshot_id=e['snapshot_id'], sandbox_id=e['sandbox_id'], restore_step=e['step'],
                       restore_phase=e['phase'], manifest_sha256=b['descriptor']['index_sha256'],
                       **prefetch(logs, e['sandbox_id'], planned))
        return {}


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('phase', choices=('checkpoint', 'restore', 'inspection', 'audit'))
    p.add_argument('service', type=Path)
    p.add_argument('output', type=Path)
    p.add_argument('url')
    p.add_argument('sandbox', type=uid)
    p.add_argument('snapshot', type=uid)
    p.add_argument('step', type=int)
    a = p.parse_args()
    pilot = Pilot(a)
    action = {'checkpoint': pilot.checkpoint, 'restore': pilot.restore, 'inspection': pilot.restore, 'audit': pilot.audit}
    print(json.dumps(action[a.phase]()), flush=True)
