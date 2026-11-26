#!/usr/bin/env python3
"""Capture stock AgentENV RAM at ARIES boundaries. Run on the replay worker.

Compile tb41/native-deep-scan.c with cc -O2 -pthread. Start this collector
in a private run directory, then pass RUN/boundary.sock to -boundary-socket.
BINDINGS follows host_sample.py's workload socket and generation contract.
"""
import argparse
from collections import Counter
from contextlib import ExitStack
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import time

from host_sample import bind, process_status, read_bindings
TRACE = Path(__file__).resolve().parent.parent
sys.path[:0] = [str(TRACE / 'tb41'), str(TRACE)]
from collect import digest, exchange
from idle_hook import Firecracker, signal_error
from vmm_census import census

PAGE = 4096
PHASES = {'session-start', 'session-end', 'pre-tool', 'post-tool', 'tool', 'parallel-tool', 'wait'}
CODES = ['absent at all checks', 'accessed', 'idle', 'newly-resident-unarmed',
         'became-nonresident', 'host-mapping-changed', 'pfn-masked-or-zero',
         'compound-resolution-unsupported', 'shared-or-zero-page', 'not-on-lru',
         'nonexclusive-or-transition']


def layout(data, memory_size):
    rows = sorted(data, key=lambda r: r['offset'])
    cursor = 0
    spans = []
    for r in rows:
        addr, offset, size = (r[k] for k in ('base_host_virt_addr', 'offset', 'size'))
        if (r['page_size'] != PAGE or any(type(x) is not int or x % PAGE for x in (addr, offset, size))
                or offset != cursor or size <= 0 or addr <= 0):
            raise ValueError('invalid stock RAM region')
        spans.append((addr, addr + size))
        cursor += size
    spans.sort()
    if not rows or cursor != memory_size or any(a[1] > b[0] for a, b in zip(spans, spans[1:])):
        raise ValueError('incomplete or overlapping stock RAM layout')
    return rows


def dirty_bytes(data, regions):
    memory_size = sum(r['size'] for r in regions)
    if data['page_size'] != PAGE or data['memory_size'] != memory_size:
        raise ValueError('dirty inventory disagrees with stock RAM layout')
    last_end = region_index = total = 0
    for row in sorted(data['ranges'], key=lambda r: r['image_offset']):
        offset, length, addr = (row[k] for k in ('image_offset', 'length', 'base_host_virt_addr'))
        if (any(type(n) is not int or n % PAGE for n in (offset, length, addr))
                or offset < last_end or length <= 0 or offset + length > memory_size):
            raise ValueError('invalid dirty range')
        while offset >= regions[region_index]['offset'] + regions[region_index]['size']:
            region_index += 1
        region = regions[region_index]
        if (addr != region['base_host_virt_addr'] + offset - region['offset']
                or offset + length > region['offset'] + region['size']):
            raise ValueError('dirty range does not match its stock RAM region')
        last_end = offset + length
        total += length
    return total


def page_states(proc, regions):
    # Retain the present, swapped, file/shared-anon bits only. No guest contents or host PFNs.
    compact = bytearray()
    top_byte = 7 if sys.byteorder == 'little' else 0
    with (proc / 'pagemap').open('rb', buffering=0) as pm:
        for r in regions:
            count = r['size'] // PAGE
            pm.seek(r['base_host_virt_addr'] // PAGE * 8)
            while count:
                n = min(count, 65536)
                raw = pm.read(n * 8)
                if len(raw) != n * 8:
                    raise OSError('short stock VMM pagemap read')
                compact.extend(raw[top_byte::8].translate(PTE_TABLE))
                count -= n
    return compact


PTE_TABLE = bytes(i >> 5 for i in range(256))


class Generation:
    def __init__(self, resources, binding, root, scanner, sandbox):
        self.binding = binding
        self.out = root / f'generation-{binding.generation}'
        self.out.mkdir(mode=0o700)
        proc, fd, pid, start = bind(binding.socket)
        resources.callback(os.close, fd)
        self.proc = proc
        self.api = Firecracker(str(binding.socket))
        self.api.identity = pid, str(start)
        info = self.api.request('GET', '/')
        if info['state'] != 'Running':
            raise RuntimeError('stock VMM is not running')
        self.machine = self.api.request('GET', '/machine-config')
        self.memory_size = self.machine['mem_size_mib'] << 20
        self.regions = layout(self.api.request('GET', '/vm/guest-memory-regions'), self.memory_size)
        self.slots = [dict(userspace_addr=r['base_host_virt_addr'], memory_size=r['size']) for r in self.regions]
        command = [scanner, self.out, pid, start]
        for r in self.regions:
            command.extend((r['base_host_virt_addr'], r['size']))
        stderr = resources.enter_context((self.out / 'scanner.stderr').open('x'))
        self.child = subprocess.Popen([str(x) for x in command], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                      stderr=stderr, text=True, env={**os.environ, 'ZB_SCAN_THREADS': '2'})
        resources.callback(self.stop)
        ready = exchange(self.child, None, 'ready')
        if ready['scan_threads'] != 2:
            raise RuntimeError('unexpected scanner thread count')
        sources = [Path(__file__), scanner, TRACE / 'tb41/native-deep-scan.c', TRACE / 'tb41/idle-scan.c']
        metadata = dict(schema='stock-deep-v1', clock='host-CLOCK_MONOTONIC', sandbox=sandbox,
                        generation=binding.generation, socket=str(binding.socket), vmm_info=info, pid=pid,
                        start_ticks=start, vmm_sha256=digest(proc / 'exe'), machine=self.machine,
                        ram_regions=self.regions, page_bytes=PAGE, scan_threads=2,
                        page_index='contiguous snapshot image offset divided by page_bytes',
                        pte_bits={'4': 'present', '2': 'swapped', '1': 'file or shared anonymous'},
                        idle_codes=CODES, idle_code_mask=63, absent_at_arm_mask=64, absent_at_end_mask=128,
                        dirty_basis='stock logically non-clearing dirty ranges in cumulative image layout',
                        sources={str(p): digest(p) for p in sources})
        (self.out / 'metadata.json').write_text(json.dumps(metadata) + '\n')
        self.previous_index = None

    def stop(self):
        try:
            self.child.stdin.close()
        except BrokenPipeError:
            pass
        try:
            status = self.child.wait(timeout=20)
        except subprocess.TimeoutExpired:
            self.child.kill()
            self.child.wait()
            raise
        finally:
            self.child.stdout.close()
        if status:
            raise RuntimeError(f'native deep scanner exited with {status}')

    def capture(self, event):
        api, child = self.api, self.child
        if api.request('GET', '/')['state'] != 'Running':
            raise RuntimeError('capture requires a runnable stock VMM')
        freeze = event['freeze'] = dict(pause_request_ns=time.monotonic_ns())
        try:
            api.request('PATCH', '/vm', 'Paused')
            freeze['pause_ack_ns'] = time.monotonic_ns()
            if self.regions != layout(api.request('GET', '/vm/guest-memory-regions'), self.memory_size):
                raise RuntimeError('RAM layout changed within a VMM generation')
            if self.previous_index is not None:
                event['capture'] = exchange(child, 'capture', 'captured')
            event['pte_read_begin_ns'] = time.monotonic_ns()
            raw = page_states(self.proc, self.regions)
            event['pte_read_end_ns'] = time.monotonic_ns()
            charge = census(api.identity[0], self.slots, [])
            # Stock reports only RAM regions. Outside-RAM VMAs can include KVM devices.
            charges = charge['categories']
            if 'outside_kvm' in charges:
                charges['outside_stock_ram_regions'] = charges.pop('outside_kvm')
            event['vmm_census'] = charge
            dirty = None
            if self.machine.get('track_dirty_pages') is True:
                event['dirty_request_ns'] = time.monotonic_ns()
                dirty = api.request('GET', '/vm/dirty-memory-ranges')
                event['dirty_reply_ns'] = time.monotonic_ns()
            else:
                event.update(dirty_bytes=None, dirty_unavailable='machine does not confirm dirty tracking')
            event['arm'] = exchange(child, 'arm', 'armed')
        finally:
            freeze['resume_request_ns'] = time.monotonic_ns()
            api.request('PATCH', '/vm', 'Resumed')
            freeze['resume_ack_ns'] = time.monotonic_ns()
        index = event['index']
        if self.previous_index is not None:
            window = event['window'] = exchange(child, 'export', 'exported')
            window['arm_capture_index'] = self.previous_index
            window['file'] = f"idle-{window['window']:02d}.u8"
            pages = self.memory_size // PAGE
            if sum(window['code_counts']) != pages or (self.out / window['file']).stat().st_size != pages:
                raise RuntimeError('native activity inventory does not cover the RAM image')
        else:
            event.update(window=None, window_unavailable='first observation of this VMM generation')
        event['pte_file'] = f'pte-{index:04d}.u8'
        (self.out / event['pte_file']).write_bytes(raw)
        event['pte_code_counts'] = Counter(raw)
        if dirty is not None:
            event['dirty_bytes'] = dirty_bytes(dirty, self.regions)
            event['dirty_file'] = f'dirty-{index:04d}.json'
            (self.out / event['dirty_file']).write_text(json.dumps(dirty, separators=(',', ':')) + '\n')
        self.previous_index = index


def serve(run, bindings, sandbox, scanner):
    if os.sysconf('SC_PAGE_SIZE') != PAGE:
        raise ValueError('requires a 4 KiB host')
    root = run / 'stock-deep'
    root.mkdir(mode=0o700)
    endpoint = run / 'boundary.sock'
    with ExitStack() as stack:
        server = stack.enter_context(socket.socket(socket.AF_UNIX, socket.SOCK_STREAM))
        server.bind(str(endpoint))
        stack.callback(endpoint.unlink, missing_ok=True)
        server.listen(1)
        log = stack.enter_context((root / 'boundaries.jsonl').open('x'))
        resources = stack.enter_context(ExitStack())
        generation = None
        index = 0
        while True:
            conn, _ = server.accept()
            with conn, conn.makefile('rb') as stream:
                raw = stream.readline(8193)
                if len(raw) > 8192 or not raw.endswith(b'\n'):
                    raise ValueError('incomplete stock boundary')
                phase, runtime, step, api, triggers = json.loads(raw)
                if (runtime != sandbox or api != '' or phase not in PHASES or type(step) is not int
                        or not isinstance(triggers, list) or not triggers or set(triggers) - (PHASES | {'timer'})
                        or (index == 0) != (phase == 'session-start')):
                    raise ValueError('unexpected stock boundary request')
                index += 1
                event = dict(index=index, phase=phase, step=step, triggers=triggers, sandbox=sandbox,
                             hook_begin_ns=time.monotonic_ns(), wall_ns=time.time_ns())
                try:
                    rows, error = read_bindings(bindings)
                    binding = rows.get('workload')
                    if binding is None or binding.socket is None:
                        raise RuntimeError(error or 'workload VMM binding unavailable')
                    event.update(generation=binding.generation, socket=str(binding.socket))
                    if generation is None or binding.generation != generation.binding.generation:
                        if generation is not None:
                            if binding.generation < generation.binding.generation:
                                raise RuntimeError('stale VMM generation')
                            try:
                                old_state = process_status(generation.proc)[0]
                            except (FileNotFoundError, ProcessLookupError):
                                old_state = None
                            if old_state is not None and old_state not in 'ZXx':
                                raise RuntimeError('previous workload VMM is still live')
                        resources.close()
                        generation = Generation(resources, binding, root, scanner, sandbox)
                    if binding.socket != generation.binding.socket:
                        raise RuntimeError('workload socket changed within a generation')
                    generation.capture(event)
                    current, _ = read_bindings(bindings)
                    after = current.get('workload')
                    if after is None or (after.generation, after.socket) != (binding.generation, binding.socket):
                        raise RuntimeError('workload VMM binding changed during capture')
                    event['status'] = 'ok'
                except BaseException as exc:
                    event.update(status='error', error=str(exc))
                    raise
                finally:
                    event['hook_end_ns'] = time.monotonic_ns()
                    print(json.dumps(event, separators=(',', ':')), file=log, flush=True)
                conn.sendall((json.dumps(event['freeze']) + '\n').encode())
            if phase == 'session-end':
                break


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('run', type=Path)
    parser.add_argument('bindings', type=Path)
    parser.add_argument('sandbox')
    parser.add_argument('--scanner', type=Path, required=True)
    args = parser.parse_args()
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, signal_error)
    serve(args.run.resolve(), args.bindings.resolve(), args.sandbox, args.scanner.resolve(strict=True))
