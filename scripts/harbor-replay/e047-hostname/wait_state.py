"""Read task-local wait channels without signalling or attaching to the source processes."""
import json
from pathlib import Path
import time

procs=Path('/proc')
rows=[]
for proc in procs.glob('[0-9]*'):
    try:
        argv=(proc/'cmdline').read_bytes().rstrip(b'\0').split(b'\0')
        if not argv or not Path(argv[0].decode(errors='replace')).name.startswith('python') or b'/tmp/verify_parallel_linear.py' not in argv:continue
        stat=(proc/'stat').read_text().rsplit(')',1)[1].split()
        threads=[]
        for task in (proc/'task').iterdir():
            try:threads.append(dict(tid=int(task.name),wchan=(task/'wchan').read_text().strip()))
            except (FileNotFoundError,ProcessLookupError):pass
        rows.append(dict(pid=int(proc.name),ppid=int(stat[1]),start_ticks=int(stat[19]),state=stat[0],threads=threads))
    except (FileNotFoundError,ProcessLookupError):continue
print(json.dumps(dict(guest_monotonic_ns=time.monotonic_ns(),test_processes=rows)))
