import json
import os
from pathlib import Path
import runpy
import sys
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import patch

from placement import partitions, verify_vm_inventory, vmm_comm


class PlacementTest(unittest.TestCase):
    def test_disjoint_and_overlapping_affinity(self):
        partitions({0, 1}, {2, 3}, {0, 1})
        for peer, tracer in [({1, 2}, {0}), ({2, 3}, {0, 2})]:
            with self.assertRaises(ValueError):
                partitions({0, 1}, peer, tracer)

    def test_sampler_python_entry_point(self):
        sampler = Path(__file__).with_name('placement-sampler')
        with TemporaryDirectory() as tmp:
            env = dict(ZB_TB41_UNIT='/run/service-A', ZB_TB41_RUNTIME='/runtime',
                       ZB_TB41_PEER_UNIT='service-B', ZB_CGROUP_EVENTS=tmp + '/events',
                       ZB_TB41_PLACEMENT_SAMPLER='/actual/sample.py')
            with patch.dict(os.environ, env), patch.object(sys, 'argv', [str(sampler), '/guest.sock']), \
                    patch('placement.inspect', return_value={'service':'service-A'}) as inspect, \
                    patch('os.execv') as execute:
                runpy.run_path(str(sampler), run_name='__main__')
            inspect.assert_called_once_with('service-A', Path('/runtime'), 'service-B', restored_api=Path('/guest.sock'))
            execute.assert_called_once_with(sys.executable, [sys.executable, '-u', '/actual/sample.py', '/guest.sock'])
            self.assertEqual(json.loads((Path(tmp) / 'placement-live.json').read_text()), {'service':'service-A'})

    def test_created_and_restored_vm_process_names(self):
        for name in ['firecracker', '.firecracker-re', 'qemu-system-x86']:
            self.assertTrue(vmm_comm(name))
        self.assertFalse(vmm_comm('firecracker-cont'))

    def test_foreign_and_reused_pid(self):
        live = {(14, 500), (18, 600)}
        verify_vm_inventory(live, live)
        for owners in [{(14, 500)}, {(14, 500), (18, 599)}]:
            with self.assertRaises(ValueError):
                verify_vm_inventory(live, owners)
