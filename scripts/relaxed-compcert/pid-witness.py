"""Diagnostic guest-only pidfd witness for the one named installer in the 921a original."""
import ctypes
import json
import os
from pathlib import Path
import select
import sys
import time

PATH=Path('/run/aries-compcert-921a-installer')
EXPECTED=['opam','install','--switch=compcert-3.13.1','--jobs=2','coq.8.16.1','menhir.20230608','dune.3.15.3']
def identity(pid):
    raw=Path('/proc')/str(pid)
    stat=(raw/'stat').read_text()
    rest=stat[stat.rindex(')')+2:].split()
    return dict(pid=pid,ppid=int(rest[1]),start_ticks=int(rest[19]),comm=(raw/'comm').read_text().strip(),
        argv=(raw/'cmdline').read_bytes().rstrip(b'\0').decode().split('\0'),exe=os.readlink(raw/'exe'),
        pid_namespace=os.readlink(raw/'ns/pid'),boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip())
def persisted(name,r):
    p=PATH/name
    t=p.with_suffix('.new');t.write_text(json.dumps(r)+'\n');t.replace(p)
def pidfd_open(pid):
    if hasattr(os,'pidfd_open'):return os.pidfd_open(pid)
    assert os.uname().machine=='x86_64'
    libc=ctypes.CDLL(None,use_errno=True)
    fd=libc.syscall(434,pid,0)
    if fd<0:raise OSError(ctypes.get_errno(),os.strerror(ctypes.get_errno()))
    return fd
def main(phase,pid):
    if phase=='arm':
        PATH.mkdir()
        fd=pidfd_open(pid)
        who=identity(pid)
        assert who['comm']==EXPECTED[0] and who['argv']==EXPECTED and Path(who['exe']).name==EXPECTED[0]
        poll=select.poll();poll.register(fd,select.POLLIN)
        assert not poll.poll(0)
        armed=dict(**who,armed_monotonic_ns=time.monotonic_ns())
        persisted('armed',armed)
        if os.fork()==0:
            os.setsid()
            with open(os.devnull,'r+b',buffering=0) as null:
                for dest in (0,1,2):os.dup2(null.fileno(),dest)
            flags=poll.poll()
            persisted('terminal',dict(pid=pid,start_ticks=who['start_ticks'],pidfd_poll_flags=flags[0][1],observed_exit_monotonic_ns=time.monotonic_ns()))
            os._exit(0)
        os.close(fd)
        return armed
    armed=json.loads((PATH/'armed').read_bytes())
    assert pid==armed['pid']
    try:current=identity(pid)
    except (FileNotFoundError,ProcessLookupError):current=None
    if current is not None:assert current=={k:v for k,v in armed.items() if k!='armed_monotonic_ns'}
    terminal=json.loads((PATH/'terminal').read_bytes()) if (PATH/'terminal').exists() else None
    if terminal:
        assert terminal['pid']==pid and terminal['start_ticks']==armed['start_ticks'] and terminal['pidfd_poll_flags']&select.POLLIN
    if phase in ('closed','source_final'):assert terminal, 'native installer session closed without a matching pidfd exit witness'
    return dict(phase=phase,armed=armed,current=current,terminal=terminal,observed_monotonic_ns=time.monotonic_ns())
if __name__=='__main__':print(json.dumps(main(sys.argv[1],int(sys.argv[2]))))
