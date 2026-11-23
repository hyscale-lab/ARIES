"""Check both Firecracker vCPU threads at the runtime's actual API socket."""
import json
import os
from pathlib import Path
import socket
import struct
import sys

path = Path(sys.argv[1])
expected = int(sys.argv[2])
fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
try:
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.connect(f"/proc/self/fd/{fd}/{path.name}")
        pid, _, _ = struct.unpack("3i", conn.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
finally:
    os.close(fd)
process = Path("/proc") / str(pid)
threads = []
for thread in sorted((process / "task").iterdir()):
    stat = (thread / "stat").read_text().rsplit(")", 1)[1].split()
    threads.append(dict(tid=int(thread.name), name=(thread / "comm").read_text().strip(),
                        allowed=sorted(os.sched_getaffinity(int(thread.name))), last_cpu=int(stat[36])))
vcpu = [t for t in threads if "vcpu" in t["name"].lower()]
cpu = Path("/sys/devices/system/cpu")
cg = (process / "cgroup").read_text().strip().split("::", 1)[1]
effective = (Path("/sys/fs/cgroup") / cg.lstrip("/") / "cpuset.cpus.effective").read_text().strip()
data = dict(pid=pid, exe=os.readlink(process / "exe"), threads=threads, vcpu_threads=vcpu,
            cgroup=cg, cpuset_effective=effective, host_cpu=expected,
            socket=(cpu / f"cpu{expected}/topology/physical_package_id").read_text().strip(),
            core=(cpu / f"cpu{expected}/topology/core_id").read_text().strip(),
            thread_siblings=(cpu / f"cpu{expected}/topology/thread_siblings_list").read_text().strip(),
            host_smt_active=(cpu / "smt/active").read_text().strip())
print(json.dumps(data), flush=True)
assert len(vcpu) == 2 and effective == str(expected), data
assert all(t["allowed"] == [expected] and t["last_cpu"] == expected for t in vcpu), data
