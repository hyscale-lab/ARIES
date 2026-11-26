import unittest

from placement import partitions, verify_vm_inventory


class PlacementTest(unittest.TestCase):
    def test_disjoint_and_overlapping_affinity(self):
        partitions({0, 1}, {2, 3}, {0, 1})
        for peer, tracer in [({1, 2}, {0}), ({2, 3}, {0, 2})]:
            with self.assertRaises(ValueError):
                partitions({0, 1}, peer, tracer)

    def test_foreign_and_reused_pid(self):
        live = {(14, 500), (18, 600)}
        verify_vm_inventory(live, live)
        for owners in [{(14, 500)}, {(14, 500), (18, 599)}]:
            with self.assertRaises(ValueError):
                verify_vm_inventory(live, owners)
