import struct
import unittest

from pack import PAGE, classify, pages


class PackTest(unittest.TestCase):
    def test_prefix_budget_gpa_hole_and_unknown(self):
        manifest = struct.pack("<8sIIQIIQQQQ", b"AENVMF01", 4, 32, 8*PAGE, 2, 1,
                               PAGE, PAGE, 0, 5*PAGE)
        self.assertEqual(list(pages(manifest, 3*PAGE+1)), [PAGE, 0, 2*PAGE])
        slots = [dict(image_offset=PAGE, memory_size=2*PAGE, guest_phys_addr=100*PAGE)]
        guest = dict(page_bytes=PAGE, pfn_scan=dict(ranges=[
            dict(start_pfn=100,end_pfn_exclusive=101,**{"class":"anonymous-flagged"}),
            dict(start_pfn=101,end_pfn_exclusive=102,**{"class":"file-cache-candidate-clean"})]))
        result = classify(manifest, 4*PAGE, slots, guest)
        self.assertEqual(result["planned_payload_bytes"], 4*PAGE)
        self.assertEqual(result["class_bytes"]["unknown"], 3*PAGE)
        self.assertEqual(result["file_cache_candidate_bytes"], PAGE)
        self.assertEqual(sum(result["class_shares"].values()), 1)

    def test_overlap_rejected_before_budget(self):
        bad = struct.pack("<8sIIQIIQQQQ", b"AENVMF01", 4, 32, 2*PAGE, 0, 2, 0,PAGE,0,PAGE)
        with self.assertRaises(ValueError):
            list(pages(bad, 0))
