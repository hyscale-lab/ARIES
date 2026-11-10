#!/usr/bin/env python3
"""Actual host UFFD/SCM_RIGHTS/COPY smoke; run as root on the worker only."""
import argparse
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import time
from control import send


def line(proc):
    ready,_,_=select.select([proc.stdout],[],[],15)
    if not ready:raise TimeoutError('Native fixture response timeout')
    value=proc.stdout.readline().strip()
    if not value:raise RuntimeError('Native fixture exited')
    return value


def main():
    ap=argparse.ArgumentParser();ap.add_argument('--output',type=Path,required=True);a=ap.parse_args()
    root=a.output.resolve();root.mkdir(parents=True,exist_ok=False);root.chmod(0o700)
    scripts=Path(__file__).resolve().parent
    subprocess.run(['cc','-O2','-Wall','-Wextra','-Werror','-o',str(root/'fixture'),str(scripts/'native_fixture.c')],check=True)
    (root/'memory').write_bytes(b''.join(bytes([v])*4096 for v in [17,34,51,68]))
    sock=str(root/'uffd.sock');ctl=str(root/'control.sock')
    handler=None;fixture=None
    with (root/'handler.stderr').open('w') as err, (root/'fixture.stderr').open('w') as ferr:
        try:
            handler=subprocess.Popen([sys.executable,str(scripts/'handler.py'),'--socket',sock,'--control',ctl,'--memory',str(root/'memory'),'--log',str(root/'events.jsonl')],stderr=err)
            deadline=time.monotonic()+10
            while not Path(ctl).exists():
                if handler.poll() is not None or time.monotonic()>deadline:raise RuntimeError('Handler failed to listen')
                time.sleep(.02)
            fixture=subprocess.Popen([str(root/'fixture'),sock],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=ferr,text=True,bufsize=1)
            assert line(fixture)=='ready'
            send(ctl,'phase','tool','call-1')
            fixture.stdin.write('1\n');fixture.stdin.flush();assert line(fixture)=='first 17 17 99'
            send(ctl,'phase','tool','call-2')
            fixture.stdin.write('2\n');fixture.stdin.flush();assert line(fixture)=='second 51 17'
            send(ctl,'phase','snapshot',None)
            fixture.stdin.write('q\n');fixture.stdin.flush();fixture.wait(timeout=10);assert fixture.returncode==0
            handler.wait(timeout=10);assert handler.returncode==0
            rows=[json.loads(s) for s in (root/'events.jsonl').read_text().splitlines()]
            faults=[r for r in rows if r['event']=='pagefault']
            assert [r['snapshot_page'] for r in faults]==[0,1,2],faults
            assert [r['first_fault_write_flag'] for r in faults]==[False,True,False],faults
            assert [r['tool_id'] for r in faults]==['call-1','call-1','call-2'],faults
            assert all(r['result']=='copied' for r in faults)
            assert rows[-1]['event']=='summary' and not rows[-1]['failed']
            result={'status':'passed','actual_uffd_copy_pages':3,'retouch_created_no_extra_fault':True,'first_fault_write_flags_verified_for_host_fixture_only':True,'phase_ids_verified':True,'snapshot_bytes_preserved':True,'scope':'host synthetic fixture; not KVM/Firecracker validation'}
            (root/'result.json').write_text(json.dumps(result,indent=2)+'\n');print(json.dumps(result))
        finally:
            for proc in [fixture,handler]:
                if proc is not None and proc.poll() is None:
                    proc.kill();proc.wait(timeout=10)


if __name__=='__main__':main()
