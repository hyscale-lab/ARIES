import unittest

from analyze import GIB, NS, integrate


class IntegralTest(unittest.TestCase):
    def test_interpolated_constant_and_partial_endpoints(self):
        points = [(0, 0), (NS, GIB), (2 * NS, GIB), (3 * NS, 0)]
        result = integrate(points, NS // 2, 5 * NS // 2)
        self.assertAlmostEqual(result["pss_gib_seconds"], 1.75)
        self.assertEqual(result["peak_pss_sampled_bytes"], GIB)
        self.assertEqual(result["interior_sample_count"], 2)
        self.assertEqual(result["max_covered_segment_ns"], NS)

    def test_phase_outside_sampling_cannot_be_interpolated(self):
        with self.assertRaisesRegex(ValueError, "outside"):
            integrate([(1, 0), (2, 1)], 0, 2)


if __name__ == "__main__":
    unittest.main()
