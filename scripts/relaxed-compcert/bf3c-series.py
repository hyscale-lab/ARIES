"""Run the sealed ordinary driver with one trial-specific OPAM upstream extension."""
import hashlib
from pathlib import Path
import subprocess
import sys

STAGE=Path('/local/zb-relaxed-compcert-01')
SHA='ee51d9265b39b1c79ea21e0e0ab5613d0017139eef79a706d8ec370aa71d0a86'
TRIAL='bf3c1acf-2d7d-4fa4-9b03-fb8edfce1666'
def render(src,recovery):
    assert hashlib.sha256(src).hexdigest()==SHA
    text=src.decode()
    if recovery:
        text=text.replace('out="$stage/$variant"','out="$stage/fallback-bf3c-$variant-01"')
    start='    python3 -B audit-idle-runtime.py "$unit" "$rt" > "$p/idle-before.json"'
    end='    printf \'%s %s %s %s\\n\' "$node" "$variant" "$id" "$rc" >> "$out/series-status.txt"'
    assert text.count(start)==text.count(end)==1
    return text.replace(start, f'    if [[ $id == {TRIAL} ]]; then python3 -B "$stage/bf3c-fallback-01.py" enable "$p/menhir-upstream"; fi\n'+start).replace(end,
        f'    if [[ $id == {TRIAL} ]]; then python3 -B "$stage/bf3c-fallback-01.py" disable "$p/menhir-upstream"; fi\n'+end)
if __name__=='__main__':
    args=sys.argv[1:]
    recovery=args.pop(0)=='recovery'
    assert Path(args[0])==STAGE and (not recovery or args[6:]==[TRIAL])
    subprocess.run(['bash','-s','--',*args],input=render((STAGE/'run-series.sh').read_bytes(),recovery),text=True,check=True)
