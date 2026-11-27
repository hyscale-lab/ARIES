#!/usr/bin/env python3
"""Exercise the joined setup/replay offsets, frame buckets, idle states and archive roundtrip."""
import importlib.util
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import patch

import numpy as np


def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + '.py'))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


s = module('cow-sample')
r = module('cow-reduce')
# Offsets: shared, four frames, external, zero, alias, other offset alias,
# one masked PFN, unstable, setup-dirty and replay-dirty, unresolved metadata.
raw = np.array([[1,2,6,8,9,9,0,13,15,18], [1,3,7,8,10,0,12,14,16,19],
                [0,4,0,0,0,0,0,0,17,0], [0,5,0,0,0,0,0,0,0,0]], dtype='u8')
ptes = np.where(raw != 0, raw | s.PRESENT, np.uint64(0))
ptes[0,6] = s.PRESENT
ids = np.unique(raw[raw != 0])
counts = np.array([(raw == f).sum() for f in ids], dtype='u8')
counts[ids == 7] += 1
counts[ids == 19] = s.UNKNOWN
flags = np.where(ids == 8, np.uint64(s.ZERO), np.uint64(1 << 5))
stable = np.ones_like(raw, dtype=bool)
stable[0,7] = False
setup = np.zeros_like(stable)
setup[0,8] = True
replay = np.zeros_like(stable)
replay[1,8] = True
idle = np.zeros_like(raw, dtype='u1')
idle[:,1] = 2
data = dict(ptes=ptes, pfns=ids, flags=flags, mapcount=counts,
            unstable_pfns=np.array([13], dtype='u8'), stable=np.packbits(stable, axis=1), idle=idle, replay_dirty=np.packbits(replay, axis=1))
with TemporaryDirectory() as tmp:
    file = Path(tmp) / 'test.npz'
    s.save_arrays(file, data)
    with np.load(file) as loaded:
        result = r.reduce_arrays(loaded, setup)
    assert result['logical_pages'] == 9, result
    assert result['already_shared_pages'] == 2 and result['pfn_d_minus_one_pages'] == 6
    assert result['ordinary_d_minus_one_pages'] == result['all_idle_ordinary_pages'] == 3
    assert result['zero_logical_pages'] == result['external_d_minus_one_pages'] == 1
    assert result['other_cohort_alias_d_minus_one_pages'] == result['masked_logical_pages'] == 1
    assert result['frame_metadata_unresolved_d_minus_one_pages'] == 1
    assert result['unstable_or_unresolved_offsets'] == 1 and result['k1_offsets'] == 2
    blank = dict(data, ptes=np.zeros_like(ptes), pfns=ids[:0], flags=flags[:0], mapcount=counts[:0])
    assert r.reduce_arrays(blank, setup)['logical_pages'] == 0
    ledger = Path(tmp) / 'frames'
    ledger.write_bytes(np.arange(9000, dtype='u8').tobytes())
    select = np.array([0, 50, 4095, 4100, 8700], dtype='u8')
    actual, err = s.frame_info(ledger, select)
    assert err is None and np.array_equal(select, actual)
    # Install a regular file as mock kernel idle words. Verify that only target
    # bits are submitted, including a word spanning the second sysfs block.
    bitmap = Path(tmp) / 'bitmap'
    bitmap.write_bytes(bytes(8192))
    pfns = np.array([63, 64, 67, 32770], dtype='u8')
    opener = lambda _path, *args, **kwargs: bitmap.open(*args, **kwargs)
    with patch('builtins.open', opener):
        s.idle_bitmap(pfns, True)
        assert s.idle_bitmap(pfns, False).all()
        assert not s.idle_bitmap(pfns + 1, False)[1:].any()
    class Guest:
        def __init__(self, pte):
            self.pte = pte

        def capture(self):
            return self.pte, {}, {}

        def pages(self):
            return self.pte

    def metadata(path, pfns):
        return np.full(len(pfns), (1 << 5) if path.endswith('flags') else 1, dtype='u8'), None
    with ThreadPoolExecutor(4) as pool, patch.object(s, 'frame_info', metadata), patch.object(s, 'idle_bitmap') as observer:
        out, _, _, _, errors = s.snapshots([Guest(pte) for pte in ptes], pool, None, {}, False)
        assert np.array_equal(out['ptes'], ptes) and not out['idle'].any() and not any(errors.values())
        observer.assert_not_called()
    print('cohort partitions, dirty masks, masked/zero/external/alias/idle, and archive roundtrip passed')
