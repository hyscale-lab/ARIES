"""Adapt the released direct init for the read-only Roy fixture."""
import sys
from pathlib import Path
p = Path(sys.argv[1]).read_text()
old = 'cp /usr/local/lib/deltabox-direct/check.py "$root/tmp/check.py"'
new = '''mkdir -p "$root/mnt/roy"
mount -t ext4 -o ro /dev/vdb "$root/mnt/roy"
cp /usr/local/lib/deltabox-direct/guest.py "$root/tmp/check.py"
cp /usr/local/lib/deltabox-direct/roy-plan.json "$root/tmp/plan.json"
'''
assert p.count(old) == 1
Path(sys.argv[2]).write_text(p.replace(old, new))
