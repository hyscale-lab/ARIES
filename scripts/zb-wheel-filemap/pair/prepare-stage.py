"""Add the sealed paired protocol and observer to the shared instrumented stock v3 stage."""
from pathlib import Path
import hashlib
import runpy
import shutil
import subprocess
import sys

here=Path(__file__).resolve().parent
sys.argv[0]=str(here.parent/'prepare-stage.py')
runpy.run_path(sys.argv[0],run_name='__main__')
stage=Path(sys.argv[1])
for name in ('run-source.py','observer.py','protocol.json','run-series.sh','present-bits.c'):
    shutil.copy2(here/name,stage/name)
subprocess.run(['gcc','-std=c11','-O2','-Wall','-Wextra','-Werror','-o',stage/'present-bits',stage/'present-bits.c'],check=True)
def sha(path):
    with path.open('rb') as stream:return hashlib.file_digest(stream,'sha256').hexdigest()
files=sorted(p for p in stage.iterdir() if p.name!='SHA256SUMS')
(stage/'SHA256SUMS').write_text(''.join(f'{sha(p)}  {p.name}\n' for p in files))
print(sha(stage/'SHA256SUMS'),stage/'SHA256SUMS')
