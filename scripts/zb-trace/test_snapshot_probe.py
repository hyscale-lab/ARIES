import hashlib
from pathlib import Path
import random
import tempfile
import unittest

from snapshot_probe import PAGE, sample_snapshot, selected


class PackedMemoryTests(unittest.TestCase):
    def test_discontiguous_guest_ram_uses_packed_file_offsets(self):
        slots = [dict(guest_phys_addr=9*PAGE,memory_size=PAGE,userspace_addr=30*PAGE),
                 dict(guest_phys_addr=0,memory_size=2*PAGE,userspace_addr=10*PAGE)]
        self.assertEqual(selected(slots,3),[(0,10*PAGE,0),(1,11*PAGE,PAGE),(9,30*PAGE,2*PAGE)])
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"memory"
            pages = [bytes([i])*PAGE for i in (10,20,30)]
            path.write_bytes(b"".join(pages))
            self.assertEqual(sample_snapshot(path,slots,3),
                             [dict(gfn=n,digest=hashlib.sha256(p).hexdigest()) for n,p in zip((0,1,9),pages)])
            path.write_bytes(pages[0])
            with self.assertRaises(ValueError):
                sample_snapshot(path,slots,3)

    def test_single_region_retains_original_sampling_frame(self):
        slots = [dict(guest_phys_addr=0,memory_size=4096*PAGE,userspace_addr=PAGE)]
        self.assertEqual([g for g,_,_ in selected(slots,300)],sorted(random.Random(693587).sample(range(4096),300)))
