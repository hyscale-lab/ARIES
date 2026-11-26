import json
import struct
from pathlib import Path
import tempfile
import unittest

from checkpoint_classes import composition, image_slots
from checkpoint import differential, prefetch, recorder_socket, workload_socket


def layer(n, size):
    return dict(digest=f'sha256:{n:064x}', size=size)


class StockCheckpointTest(unittest.TestCase):
    def test_differential_counts_each_new_managed_blob_once_across_all_drives(self):
        inherited, cached, disk, memory = [layer(i, 4*i) for i in range(1, 5)]
        parent = dict(committed=dict(rootfs_layers=[{'Managed': inherited}], attached_drives=[],
                                     volume_snapshots=[], memory_layers=[]))
        child = dict(id='child', committed=dict(rootfs_layers=[{'Managed': inherited}, {'Managed': disk}],
                     attached_drives=[{'Overlaybd': {'layers': [{'Managed': disk}, {'External': {'digest': 'remote'}}]}}],
                     volume_snapshots=[{'layers': [{'Managed': cached}]}], memory_layers=[memory]))
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root/'managed-layers').mkdir()
            for ref in (disk, memory):
                (root/'managed-layers'/(ref['digest'].replace(':', '_')+'.overlaybd.commit')).write_bytes(bytes(ref['size']))
            (root/'snapshots/child').mkdir(parents=True)
            for n in ('vm_state.bin', 'firecracker-manifest.json'):
                (root/'snapshots/child'/n).write_bytes(b'abc')
            before = {cached['digest'].replace(':', '_')+'.overlaybd.commit'}
            report = differential(root, before, parent, child)
            self.assertEqual(report['differential_layer_bytes'], disk['size'] + memory['size'])
            self.assertEqual(report['differential_snapshot_bytes'], disk['size'] + memory['size'] + 6)

    def test_pack_attribution_preserves_gpa_hole_and_missing_census_denominator(self):
        ram = [dict(base_host_virt_addr=4096, page_size=4096, offset=0, size=4096, guest_phys_addr=0),
               dict(base_host_virt_addr=16384, page_size=4096, offset=4096, size=4096, guest_phys_addr=40960)]
        regs = [dict(userspace_addr=r['base_host_virt_addr'], memory_size=r['size'], guest_phys_addr=r['guest_phys_addr'])
                for r in ram]
        slots = image_slots(ram, 8192, regs)
        self.assertEqual(image_slots(ram, 8192), slots)
        with self.assertRaises(ValueError):
            image_slots(ram, 8192, regs[:1])
        with self.assertRaises(ValueError):
            image_slots([ram[0], dict(ram[1], guest_phys_addr=0)], 8192)
        blob = struct.pack('<8sIIQIIQQ', b'AENVMF01', 4, 32, 8192, 0, 1, 0, 8192)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            census = dict(schema='aries.zb.guest.snapshot.v1', page_bytes=4096, source_errors={},
                          begin_monotonic_ns=10, end_monotonic_ns=20,
                          pfn_scan=dict(requested_start_pfn=0, requested_pfn_count=11, state='complete',
                                        ranges=[{'start_pfn':10,'end_pfn_exclusive':11,'class':'slab-flagged'}]))
            (path/'pre.json').write_text(json.dumps(census))
            obs = dict(census='pre.json', requested_pfn_count=11, slots=slots)
            result = composition(path, blob, 8192, obs)
            self.assertEqual((result['class_bytes']['slab'], result['class_bytes']['unknown']), (4096, 4096))
            (path/'pre.json').unlink()
            self.assertEqual(composition(path, blob, 8192, obs)['class_bytes']['unknown'], 8192)

    def test_recording_socket_and_one_exclusive_prefetch(self):
        def row(msg, **fields):
            return dict(timestamp='2026-01-01T00:00:00Z', fields=dict(message=msg, **fields))
        started = dict(row('creating sandbox'), span={'sandbox_id': 'new'})
        drained = row('memory device startup prefetch drained', requested_bytes=8192, read_bytes=4096)
        launch = dict(row('starting sandbox from snapshot config', fc_cwd='/work/new', vm_state_path='/store/new/state'),
                      spans=[{'sandbox_id': 'new'}])
        rec = row('starting sandbox from snapshot config', fc_cwd='/work/trial', vm_state_path='/temp/old/tmp/vm_state.bin')
        events = [started, launch, drained, row('startup pack recording started', dir='/temp/old/tmp'), rec]
        self.assertEqual(recorder_socket(events, 'old'), '/work/trial/firecracker.socket')
        self.assertEqual(workload_socket(events, 'new'), '/work/new/firecracker.socket')
        self.assertEqual(prefetch(events, 'new', 8192)['read_bytes'], 4096)
        for extra in (started, drained, row('resuming sandbox')):
            with self.assertRaises(ValueError):
                prefetch(events + [extra], 'new', 8192)
        with self.assertRaises(ValueError):
            prefetch(events, 'new', 4096)


if __name__ == '__main__':
    unittest.main()
