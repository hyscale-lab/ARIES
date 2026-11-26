"""Check whether MusicXML ZIPs reproduce each target hash by changing only the six DOS timestamps."""
from datetime import datetime, timedelta
from hashlib import sha256
from pathlib import Path
from struct import pack_into
from zipfile import ZipFile
import argparse
import json


def check(sample, target, day):
    data=bytearray(sample.read_bytes())
    with ZipFile(sample) as z:
        files=z.infolist()
        assert [f.filename for f in files]==['mimetype','META-INF/container.xml','score.musicxml']
        positions=[f.header_offset+10 for f in files]
        start=z.start_dir
        for f in files:
            assert data[start:start+4]==b'PK\x01\x02'
            positions.append(start+12)
            start+=46+len(f.filename.encode())+len(f.extra)+len(f.comment)
        members={f.filename:sha256(z.read(f)).hexdigest() for f in files}
    dos_day=((day.year-1980)<<9)|(day.month<<5)|day.day
    for off in positions:pack_into('<H',data,off+2,dos_day)
    for tick in range(43200):
        value=((tick//1800)<<11)|(((tick%1800)//30)<<5)|(tick%30)
        for off in positions:pack_into('<H',data,off,value)
        if sha256(data).hexdigest()==target:
            return dict(target_sha256=target,zip_time=(day+timedelta(seconds=tick*2)).isoformat(),
                        archive_matches_with_timestamp_only=True,member_sha256=members)
    raise ValueError('no timestamp reproduced the full target hash on the given day')


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('sample',type=Path)
    p.add_argument('target')
    p.add_argument('day',type=datetime.fromisoformat)
    a=p.parse_args()
    print(json.dumps(check(a.sample,a.target,a.day),indent=2))
