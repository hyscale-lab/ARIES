#!/usr/bin/env python3
"""Per-window page-idle observations of captured KVM RAM slots.

Batching is inspired by Brendan Gregg's wss-v2 (Apache-2.0), inspected at
https://github.com/brendangregg/wss/blob/master/wss-v2.c. This implementation
reads only requested metadata runs, not the entire physical-memory bitmap.
It reports combined read-or-write evidence, not separate access directions.
"""
import collections
import os
import struct
import time

PAGE = 4096
U64 = struct.Struct('=Q')
PRESENT = 1 << 63
PFNMASK = (1 << 55) - 1
LRU = 1 << 5
COMPOUND = (1 << 15) | (1 << 16) | (1 << 17) | (1 << 22)
SHARED = (1 << 21) | (1 << 24)
MAX_WORDS = 8192
# kernfs caps binary sysfs reads/writes at one host page; /proc allows larger I/O.
BITMAP_WORDS = PAGE // U64.size


def runs(indices, limit=MAX_WORDS):
    """Contiguous requested word runs; never read intervening physical holes."""
    start = end = None
    for n in sorted(set(indices)):
        if start is not None and (n != end or n - start >= limit):
            yield start, end
            start = None
        if start is None:
            start = n
        end = n + 1
    if start is not None:
        yield start, end


def words(fd, indices, limit=MAX_WORDS):
    result = {}
    for lo, hi in runs(indices, limit=limit):
        data = os.pread(fd, (hi-lo)*8, lo*8)
        if len(data) != (hi-lo)*8:
            raise OSError('short kernel metadata read')
        result.update((lo+i, v[0]) for i, v in enumerate(struct.iter_unpack('=Q', data)))
    return result


def write_words(fd, values):
    for lo, hi in runs(values, limit=BITMAP_WORDS):
        data = b''.join(U64.pack(values[i]) for i in range(lo, hi))
        if os.pwrite(fd, data, lo*8) != len(data):
            raise OSError('short idle bitmap write')


def page_ranges(pages):
    return [[lo*PAGE, hi*PAGE] for lo, hi in runs((p//PAGE for p in pages), limit=1 << 60)]


def start_identity(pid):
    with open(f'/proc/{pid}/stat') as f:
        return f.read().rsplit(')', 1)[1].split()[19]


class IdleObserver:
    def __init__(self, pid, vm_id, slots):
        if os.sysconf('SC_PAGE_SIZE') != PAGE:
            raise ValueError('requires 4096-byte host pages')
        self.pid, self.vm_id = pid, vm_id
        self.start = start_identity(pid)
        self.slots = sorted(slots, key=lambda s: s['guest_phys_addr'])
        for key in ('guest_phys_addr', 'userspace_addr'):
            end = -1
            for s in sorted(self.slots, key=lambda s: s[key]):
                if any(s[k] < 0 or s[k] % PAGE for k in ('guest_phys_addr', 'userspace_addr', 'memory_size')) or s['memory_size'] == 0:
                    raise ValueError('invalid KVM RAM region')
                if s[key] < end:
                    raise ValueError('overlapping KVM RAM regions')
                end = s[key] + s['memory_size']
        if not self.slots:
            raise ValueError('no KVM RAM slots')
        self.fds = []
        self.armed = None
        try:
            self.pm = self._open(f'/proc/{pid}/pagemap', os.O_RDONLY)
            self.flags = self._open('/proc/kpageflags', os.O_RDONLY)
            self.count = self._open('/proc/kpagecount', os.O_RDONLY)
            self.idle = self._open('/sys/kernel/mm/page_idle/bitmap', os.O_RDWR)
        except BaseException:
            self.close()
            raise

    def _open(self, path, mode):
        fd = os.open(path, mode)
        self.fds.append(fd)
        return fd

    def close(self):
        for fd in self.fds:
            os.close(fd)
        self.fds = []

    def identity(self):
        if start_identity(self.pid) != self.start:
            raise RuntimeError('VMM identity changed')

    def inventory(self):
        self.identity()
        resident, absent_ranges = {}, []
        absent_start = absent_end = None
        for slot in self.slots:
            size = slot['memory_size']//PAGE
            base = slot['userspace_addr']//PAGE
            for offset in range(0, size, MAX_WORDS):
                n = min(MAX_WORDS, size-offset)
                data = os.pread(self.pm, n*8, (base+offset)*8)
                if len(data) != n*8:
                    raise OSError('short slot pagemap read')
                for i, (e,) in enumerate(struct.iter_unpack('=Q', data)):
                    gpa = slot['guest_phys_addr']+(offset+i)*PAGE
                    if e & PRESENT:
                        resident[gpa] = e & PFNMASK
                        if absent_start is not None:
                            absent_ranges.append([absent_start, absent_end])
                            absent_start = None
                    else:
                        if absent_start is not None and gpa != absent_end:
                            absent_ranges.append([absent_start, absent_end])
                            absent_start = None
                        if absent_start is None:
                            absent_start = gpa
                        absent_end = gpa+PAGE
        if absent_start is not None:
            absent_ranges.append([absent_start, absent_end])
        self.identity()
        return resident, absent_ranges

    def mapping(self, resident):
        pfns = {p for p in resident.values() if p}
        flags, counts = words(self.flags, pfns), words(self.count, pfns)
        out = {}
        for gpa, pfn in resident.items():
            f = flags.get(pfn, 0)
            reason = ('pfn-masked-or-zero' if not pfn else
                      'compound-resolution-unsupported' if f & COMPOUND else
                      'shared-or-zero-page' if f & SHARED else
                      'not-on-lru' if not f & LRU else
                      'nonexclusive-or-transition' if counts[pfn] != 1 else None)
            out[gpa] = {'pfn': pfn, 'unknown': reason}
        return out

    @staticmethod
    def bits(mapping):
        blocks = {}
        for v in mapping.values():
            if v['unknown'] is None:
                pfn = v['pfn']
                blocks[pfn//64] = blocks.get(pfn//64, 0) | (1 << (pfn%64))
        return blocks

    def arm(self, tool_call_id=None):
        if self.armed is not None:
            raise RuntimeError('collect before rearming')
        begin, cpu = time.monotonic_ns(), time.process_time_ns()
        resident, absent = self.inventory()
        before = self.mapping(resident)
        bitmap_write_begin = time.monotonic_ns()
        write_words(self.idle, self.bits(before))
        bitmap_write_end = time.monotonic_ns()
        self.identity()
        end = time.monotonic_ns()
        self.armed = dict(begin=begin, end=end, cpu=time.process_time_ns()-cpu,
                          before=before, absent=absent, tool_call_id=tool_call_id,
                          bitmap_write_begin=bitmap_write_begin, bitmap_write_end=bitmap_write_end)
        return {'arm_begin_ns': begin, 'arm_end_ns': end, 'arm_cpu_ns': self.armed['cpu'],
                'bitmap_write_begin_ns': bitmap_write_begin, 'bitmap_write_end_ns': bitmap_write_end,
                'resident_pages_at_arm': len(resident), 'eligible_pages_at_arm': sum(v['unknown'] is None for v in before.values())}

    def collect(self):
        if self.armed is None:
            raise RuntimeError('arm before collecting')
        a = self.armed
        begin, cpu = time.monotonic_ns(), time.process_time_ns()
        resident, _ = self.inventory()
        after = self.mapping(resident)
        bitmap_read_begin = time.monotonic_ns()
        idle_words = words(self.idle, self.bits(a['before']), limit=BITMAP_WORDS)
        bitmap_read_end = time.monotonic_ns()
        resident_final, absent_final = self.inventory()
        final = self.mapping(resident_final)
        self.identity()
        touched, idle, unknown = [], [], {}
        for gpa in sorted(a['before'].keys() | after.keys() | final.keys()):
            prior, mid, last = a['before'].get(gpa), after.get(gpa), final.get(gpa)
            if prior is None:
                reason = 'newly-resident-unarmed'
            elif mid is None or last is None:
                reason = 'became-nonresident'
            elif prior['pfn'] != mid['pfn'] or prior['pfn'] != last['pfn']:
                reason = 'host-mapping-changed'
            else:
                reason = prior['unknown'] or mid['unknown'] or last['unknown']
            if reason:
                unknown[str(gpa)] = reason
            elif idle_words[prior['pfn']//64] & (1 << (prior['pfn']%64)):
                idle.append(gpa)
            else:
                touched.append(gpa)
        end, cpu_end = time.monotonic_ns(), time.process_time_ns()
        self.armed = None
        return {'schema': 'aries.tsb.tool-pages.idle.v1', 'vm_instance': self.vm_id,
                'tool_call_id': a['tool_call_id'], 'vmm_pid': self.pid, 'vmm_start_ticks': self.start,
                'clock': 'host-CLOCK_MONOTONIC', 'page_bytes': PAGE,
                'arm_begin_ns': a['begin'], 'arm_end_ns': a['end'],
                'scan_begin_ns': begin, 'scan_end_ns': end,
                'bitmap_write_begin_ns': a.get('bitmap_write_begin'),
                'bitmap_write_end_ns': a.get('bitmap_write_end'),
                'bitmap_read_begin_ns': bitmap_read_begin, 'bitmap_read_end_ns': bitmap_read_end,
                'arm_cpu_ns': a['cpu'], 'scan_cpu_ns': cpu_end-cpu,
                'accessed_gpas': touched, 'idle_gpas': idle, 'unknown_gpas': unknown,
                'unknown_counts': dict(collections.Counter(unknown.values())),
                'not_resident_at_arm_gpa_ranges': a['absent'],
                'not_resident_at_end_gpa_ranges': absent_final,
                'range_end_exclusive': True,
                'coverage_scope': 'stable exclusive resident host-LRU base pages in captured KVM RAM slots',
                'access_kind': 'read-or-write combined; direction not observable',
                'caveats': ['A cleared idle bit is access evidence within the expanded arm/scan envelope, not exact command attribution',
                            'Guest kernel, background processes, and host/VMM accesses are included',
                            'KVM clear_young does not flush secondary TLBs; unchanged idle bits can miss accesses',
                            'Newly resident, disappeared, changed, compound, shared and non-LRU pages are unknown',
                            'Endpoint PFN checks cannot detect transient residency or A-B-A mapping transitions',
                            'GPA is comparable across this guest lifetime, not a stable guest object or host PFN identity']}
