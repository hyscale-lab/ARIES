"""Count cached file pages without accessing their contents, for one live VMM."""
import argparse
import ctypes
import json
import mmap
import os
from pathlib import Path
import stat
import time
from vmm_census import mappings


def cached(fd, size):
    page = mmap.PAGESIZE
    pages = (size + page - 1) // page
    with mmap.mmap(fd, size, access=mmap.ACCESS_COPY) as mapping:
        ptr = ctypes.addressof(ctypes.c_char.from_buffer(mapping))
        vector = (ctypes.c_ubyte * pages)()
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.mincore(ctypes.c_void_p(ptr), ctypes.c_size_t(size), vector):
            raise OSError(ctypes.get_errno(), "mincore")
        return sum(b & 1 for b in vector) * page


def collect(pid, ticks):
    proc = Path('/proc') / str(pid)
    def identity():
        return int((proc/'stat').read_text().rsplit(')',1)[1].split()[19])
    if identity() != ticks:
        raise ValueError('stale VMM')
    found = {}
    errors = []
    entries = [(p, None) for p in (proc/'fd').iterdir()]
    for m in mappings((proc/'smaps').read_text()):
        if m['inode']:
            entries.append((proc/'map_files'/f'{m["lo"]:x}-{m["hi"]:x}', m))
    for p, bounds in entries:
        try:
            st = p.stat()
            if not stat.S_ISREG(st.st_mode) or not st.st_size:
                continue
            key = (st.st_dev,st.st_ino)
            if key not in found:
                with p.open('rb',buffering=0) as file:
                    check=os.fstat(file.fileno())
                    if (check.st_dev,check.st_ino,check.st_size) != (*key,st.st_size):
                        raise ValueError('file identity changed')
                    resident = cached(file.fileno(), st.st_size)
                    found[key] = dict(device=key[0],inode=key[1],name=Path(os.readlink(p)).name,
                                      size=st.st_size,file_cached_bytes=resident,mapped_pss=0,
                                      mapped_rss=0,mapped_anon_bytes=0)
            if bounds:
                r = found[key]
                r['mapped_pss'] += bounds['Pss']
                r['mapped_rss'] += bounds['Rss']
                r['mapped_anon_bytes'] += bounds['Anonymous']
        except (OSError,ValueError) as e:
            errors.append(dict(entry=p.name,error=str(e)))
    if identity() != ticks:
        raise ValueError('VMM identity changed')
    return dict(monotonic_ns=time.monotonic_ns(),pid=pid,start_ticks=ticks,files=list(found.values()),errors=errors)


if __name__ == '__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('samples',type=Path)
    args=parser.parse_args()
    sample=json.loads(args.samples.read_text().splitlines()[-1])
    print(json.dumps(collect(sample['pid'],int(sample['start_ticks']))))
