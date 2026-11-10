import struct
import unittest
from capture_memory import CaptureMemory, CaptureFailure, GuestAddressError, NeedPages

class Provider:
    def __init__(self, regions=None):
        self.regions=regions or [{"guest_base":0,"bytes":16<<20}]
        self.pages={}
        self.requests=[]
        self.remaining_pages=65536
    def read_pages(self,gpas):
        self.requests.append(list(gpas))
        assert len(gpas)<=256
        return {g:self.pages.get(g,bytes(4096)) for g in gpas}
    def entry(self,table,index,value):
        raw=bytearray(self.pages.get(table,bytes(4096)))
        struct.pack_into("<Q",raw,index*8,value);self.pages[table]=bytes(raw)

class MemoryTests(unittest.TestCase):
    def test_batched_frontier_and_first_copy_cache(self):
        p=Provider();m=CaptureMemory(p,prefetch_pages=1)
        m.prefetch(range(0,300*4096,4096))
        self.assertEqual([len(x) for x in p.requests],[256,44])
        p.pages[0]=b"x"*4096
        self.assertEqual(m.physical(0,4),bytes(4))
        self.assertEqual(len(p.requests),2)
    def test_cross_page_copy_and_hole_rejection_before_read(self):
        p=Provider([{"guest_base":0,"bytes":4096},{"guest_base":8192,"bytes":8192}])
        m=CaptureMemory(p)
        with self.assertRaises(GuestAddressError):m.physical(4090,20)
        self.assertEqual(p.requests,[])
        p.pages[8192]=b"a"*4096;p.pages[12288]=b"b"*4096
        self.assertEqual(m.physical(12286,4),b"aabb")
    def test_four_level_and_huge_leaf_translation(self):
        p=Provider()
        p.entry(0x1000,0,0x2003);p.entry(0x2000,0,0x3003)
        p.entry(0x3000,0,0x4003);p.entry(0x4000,1,0x9003)
        m=CaptureMemory(p,prefetch_pages=1)
        self.assertEqual(m.translate(0x1234,0x1000),0x9234)
        p2=Provider();p2.entry(0x1000,0,0x2003);p2.entry(0x2000,0,0x3003);p2.entry(0x3000,1,0x400083)
        self.assertEqual(CaptureMemory(p2,prefetch_pages=1).translate(0x201234,0x1000),0x401234)
        p3=Provider();p3.entry(0x1000,0,0x2003);p3.entry(0x2000,1,0x80000083)
        self.assertEqual(CaptureMemory(p3,prefetch_pages=1).translate(0x40001234,0x1000),0x80001234)
    def test_noncanonical_and_nonpresent_are_distinct(self):
        m=CaptureMemory(Provider(),prefetch_pages=1)
        with self.assertRaises(GuestAddressError):m.translate(1<<48,0)
        with self.assertRaises(ValueError):m.translate(0,0)
        with self.assertRaises(GuestAddressError):m.translate(0,1<<63)
        self.assertFalse(issubclass(NeedPages,ValueError))
    def test_speculative_reads_bounded_to_region(self):
        p=Provider([{"guest_base":0,"bytes":4096}]);m=CaptureMemory(p,prefetch_pages=256)
        m.physical(1,8);self.assertEqual(p.requests,[[0]])
    def test_bad_provider_response_fails_whole_capture(self):
        p=Provider();p.read_pages=lambda g:{g[0]:b"short"}
        with self.assertRaises(CaptureFailure):CaptureMemory(p).physical(0,8)

if __name__=="__main__":unittest.main()
