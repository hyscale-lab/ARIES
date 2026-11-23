"""Run the sealed ordinary driver with one trial-specific OPAM upstream extension."""
import hashlib
import ipaddress
import json
from pathlib import Path
import subprocess
import sys

STAGE=Path('/local/zb-relaxed-compcert-01')
SHA='ee51d9265b39b1c79ea21e0e0ab5613d0017139eef79a706d8ec370aa71d0a86'
TRIAL='bf3c1acf-2d7d-4fa4-9b03-fb8edfce1666'
def retire_empty_bridge():
    name='zbccsnap183'
    if not Path('/sys/class/net',name).exists(): return
    assert Path('/var/emulab/boot/nodeid').read_text().strip() in ('amd182','amd189')
    assert Path('/sys/class/net',name,'bridge').is_dir()
    assert not list(Path('/sys/class/net',name,'brif').iterdir())
    proc=subprocess.check_output(['ps','-eo','comm='],text=True).splitlines()
    assert not any(p.strip() in ('firecracker','.firecracker-re') for p in proc)
    assert subprocess.check_output(['systemctl','show','-p','ActiveState','--value','zb-ccsnap-dns-amd183'],text=True).strip()=='inactive'
    assert subprocess.run(['iptables','-w','-t','mangle','-S','ZB183CCSNAP'],capture_output=True).returncode==1
    iface,=json.loads(subprocess.check_output(['ip','-j','-4','addr','show','dev',name]))
    addr,=iface['addr_info']
    assert (addr['local'],addr['prefixlen'])==('10.239.183.1',28)
    for directory in STAGE.glob('*/cni/ipam/'+name):
        for member in directory.iterdir():
            try: ipaddress.ip_address(member.name)
            except ValueError: continue
            raise RuntimeError('Allocated guest IP still present at '+str(member))
    print('Retiring empty, detached compiler CNI bridge',iface,flush=True)
    subprocess.run(['ip','link','delete','dev',name,'type','bridge'],check=True)
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
    retire_empty_bridge()
    subprocess.run(['bash','-s','--',*args],input=render((STAGE/'run-series.sh').read_bytes(),recovery),text=True,check=True)
    retire_empty_bridge()
