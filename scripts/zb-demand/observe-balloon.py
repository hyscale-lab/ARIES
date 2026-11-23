"""Read a restored Firecracker VM's balloon configuration and guest counters."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time


def capture(socket, output):
    os.chdir(socket.parent)
    inode=socket.stat().st_ino
    with output.open("x") as stream:
        while socket.exists() and socket.stat().st_ino==inode:
            row=dict(begin_ns=time.monotonic_ns(),socket_directory=socket.parent.name,socket_inode=inode)
            for path in ("balloon","balloon/statistics"):
                result=subprocess.run(["curl","-sS","--max-time","3","--unix-socket",socket.name,
                                       "-w","\n%{http_code}","http://localhost/"+path],capture_output=True,text=True)
                if result.returncode:
                    row["terminal_read_error"]=result.stderr.strip()
                    break
                body,status=result.stdout.rsplit("\n",1)
                row[path]=dict(status=int(status),response=json.loads(body))
            row["end_ns"]=time.monotonic_ns()
            print(json.dumps(row),file=stream,flush=True)
            if "terminal_read_error" in row:
                break
            time.sleep(2)


if __name__=="__main__":
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("socket",type=Path)
    p.add_argument("output",type=Path)
    a=p.parse_args()
    capture(a.socket.resolve(strict=True),a.output.resolve())
