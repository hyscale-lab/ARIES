"""Attribute selected pack offsets to a census of the still-live source guest."""
import json
import os
from pathlib import Path
import subprocess
import time

from deep import layout
from idle_hook import Firecracker, select_slots
from pack import PAGE, classify


def image_slots(regions, size, registrations=None):
    """Join on the live API HVA and lengths. Preserve the image offsets through GPA holes."""
    result = []
    for r in layout(regions, size):
        hva, length = r['base_host_virt_addr'], r['size']
        gpa = r.get('guest_phys_addr')
        if registrations is not None:
            matched = [s for s in registrations if s['userspace_addr'] == hva and s['memory_size'] == length]
            if len(matched) != 1 or (gpa is not None and gpa != matched[0]['guest_phys_addr']):
                raise ValueError('stock API and KVM registrations disagree')
            gpa = matched[0]['guest_phys_addr']
        if type(gpa) is not int or gpa < 0 or gpa % PAGE:
            raise ValueError('unverified guest physical address')
        result.append(dict(image_offset=r['offset'], userspace_addr=hva, guest_phys_addr=gpa, memory_size=length))
    spans = sorted((s['guest_phys_addr'], s['guest_phys_addr'] + s['memory_size']) for s in result)
    if any(a[1] > b[0] for a, b in zip(spans, spans[1:])):
        raise ValueError('ambiguous GPA translations')
    return result


def collect(a, socket, generation, pid, ticks):
    obs = dict(source_sandbox_id=a.sandbox, source_generation=generation, source_pid=pid, source_start_ticks=ticks)
    if not a.pack_census:
        return dict(**obs, unavailable='page census disabled for this replay')
    obs['host_begin_ns'] = time.monotonic_ns()
    try:
        fc = Firecracker(str(socket))
        fc.identity = pid, str(ticks)
        if fc.request('GET', '/')['state'] != 'Running':
            raise ValueError('source guest is not runnable at the census')
        size = fc.request('GET', '/machine-config')['mem_size_mib'] << 20
        regions = fc.request('GET', '/vm/guest-memory-regions')
        slots = None
        if a.kvm_slot_log:
            slots, _ = select_slots(a.kvm_slot_log.read_text(), pid, ticks, 10**9 // os.sysconf('SC_CLK_TCK'), size)
        obs['slots'] = image_slots(regions, size, slots)
        obs['mapping_basis'] = 'stock region API and KVM slot registration' if slots else 'stock region API'
        count = max(r['guest_phys_addr'] + r['memory_size'] for r in obs['slots']) // PAGE
        obs['requested_pfn_count'] = count
        name = f"pre-checkpoint-{generation}-{a.sandbox}.json"
        with (a.output/name).open('xb') as out:
            obs['census_begin_ns'] = time.monotonic_ns()
            command = [a.service.parent/'cli-bundle/aenv', 'exec', a.sandbox, '/usr/bin/env', 'ZB_REGULAR_MAPS=1',
                       '/usr/local/bin/zb-guest', 'snapshot', '--pfn-count', str(count)]
            subprocess.run(command, env={**os.environ, 'XDG_CONFIG_HOME': str(a.service/'xdg')},
                           stdout=out, check=True, timeout=60)
            obs['census_end_ns'] = time.monotonic_ns()
        obs['census'] = name
    except (OSError, ValueError, RuntimeError, KeyError, subprocess.SubprocessError) as exc:
        obs['unavailable'] = str(exc)
    obs['host_end_ns'] = time.monotonic_ns()
    return obs


def composition(output, blob, budget, obs):
    reason = obs.get('unavailable')
    if not reason:
        try:
            census = json.loads((output/obs['census']).read_bytes())
            if (census['schema'] != 'aries.zb.guest.snapshot.v1' or census['pfn_scan']['requested_start_pfn'] != 0
                    or census['pfn_scan']['requested_pfn_count'] != obs['requested_pfn_count']
                    or sum(s['memory_size'] for s in obs['slots']) != int.from_bytes(blob[16:24], 'little')):
                raise ValueError('guest census or source layout disagrees with snapshot metadata')
            report = classify(blob, budget, obs['slots'], census)
            return dict(**report, basis='last post-tool source census before native snapshot',
                        guest_pfn_state=census['pfn_scan']['state'], guest_source_errors=census['source_errors'],
                        guest_census_begin_ns=census['begin_monotonic_ns'], guest_census_end_ns=census['end_monotonic_ns'])
        except (OSError, ValueError, TypeError, KeyError) as exc:
            reason = str(exc)
    report = classify(blob, budget, [], {'page_bytes': PAGE, 'pfn_scan': {'ranges': []}})
    return dict(**report, unavailable=reason)
