import struct
from pathlib import Path
from tempfile import TemporaryDirectory
import unittest
from unittest.mock import Mock, patch

import deep


def region(addr, offset, size):
    return dict(base_host_virt_addr=addr, offset=offset, size=size, page_size=4096)


class StockBoundary(unittest.TestCase):
    def test_image_coverage_and_hva_join(self):
        a, b = region(8192, 0, 8192), region(40960, 8192, 4096)
        self.assertEqual(deep.layout([b, a], 12288), [a, b])
        for rows in ([a], [a, region(8192, 8192, 4096)], [a, region(40960, 4096, 4096)]):
            with self.assertRaises(ValueError):
                deep.layout(rows, 12288)
        dirty = dict(page_size=4096, memory_size=12288, ranges=[
            dict(base_host_virt_addr=40960, image_offset=8192, length=4096),
            dict(base_host_virt_addr=12288, image_offset=4096, length=4096)])
        self.assertEqual(deep.dirty_bytes(dirty, [a, b]), 8192)
        dirty['ranges'][0]['base_host_virt_addr'] = 8192
        with self.assertRaises(ValueError):
            deep.dirty_bytes(dirty, [a, b])

    def test_capture_resume_and_full_image(self):
        with TemporaryDirectory() as tmp:
            g = deep.Generation.__new__(deep.Generation)
            g.proc = g.out = Path(tmp)
            g.regions = [region(4096, 0, 12288)]
            g.memory_size = 12288
            g.machine = dict(track_dirty_pages=False)
            g.slots, g.child, g.previous_index = [], None, None
            g.api = Mock(identity=(1, '1'))
            words = [0, (5 << 61) | 123, 2 << 61, 0]
            (g.proc / 'pagemap').write_bytes(struct.pack('=4Q', *words))
            def request(method, path, state=None):
                if (method, path) == ('GET', '/'):
                    return {'state': 'Running'}
                if (method, path) == ('GET', '/vm/guest-memory-regions'):
                    return g.regions
                if (method, path) == ('GET', '/vm/dirty-memory-ranges'):
                    return dict(page_size=4096, memory_size=12288,
                                ranges=[dict(base_host_virt_addr=4096, image_offset=0, length=4096)])
                if (method, path) == ('PATCH', '/vm') and state in ('Resumed', 'Paused'):
                    return
                self.fail((method, path))
            g.api.request.side_effect = request
            event = {'index': 1}
            def scan(child, command, expected):
                if command == 'export':
                    (g.out / f'idle-{g.previous_index:02d}.u8').write_bytes(bytes([0, 1, 2]))
                    return dict(window=g.previous_index, code_counts=[1, 1, 1] + [0] * 8)
                return {}

            with patch.object(deep, 'census', return_value={'categories': {}}), patch.object(deep, 'exchange', scan):
                g.capture(event)
                self.assertEqual((g.out / event['pte_file']).read_bytes(), bytes([5, 2, 0]))
                self.assertIsNone(event['window'])
                self.assertIsNone(event['dirty_bytes'])
                self.assertLessEqual(event['freeze']['pause_ack_ns'], event['freeze']['resume_request_ns'])
                g.machine['track_dirty_pages'] = True
                for index in (2, 3):
                    event = {'index': index}
                    g.capture(event)
                    self.assertEqual(event['window']['arm_capture_index'], index - 1)
                    self.assertEqual(event['dirty_bytes'], 4096)
                    self.assertTrue((g.out / event['dirty_file']).is_file())
                (g.proc / 'pagemap').write_bytes(b'')
                with self.assertRaisesRegex(OSError, 'short'):
                    g.capture({'index': 4})
                g.api.request.assert_called_with('PATCH', '/vm', 'Resumed')
