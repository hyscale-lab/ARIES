import os
import pathlib
import re
import struct
import subprocess

root = pathlib.Path("/sys/devices/system/cpu")
if (root / "smt/active").read_text().strip() != "0":
    raise RuntimeError("SMT active")
cpuinfo = pathlib.Path("/proc/cpuinfo").read_text().split("\n\n")[0]
for name, value in (("vendor_id", "AuthenticAMD"), ("cpu family", "23"), ("model", "49"),
                    ("model name", "AMD EPYC 7302P 16-Core Processor")):
    if re.search(rf"^{name}\s*:\s*{re.escape(value)}$", cpuinfo, re.M) is None:
        raise RuntimeError(f"unexpected CPU {name}")
online = (root / "online").read_text().strip()
cpus = []
for part in online.split(","):
    bounds = [int(x) for x in part.split("-")]
    cpus.extend(range(bounds[0], bounds[-1] + 1))
hwcr = 0xC0010015
cpb_disable = 1 << 25
p0_definition = 0xC0010064
pstate_control = 0xC0010062
fds = [os.open(f"/dev/cpu/{cpu}/msr", os.O_RDWR) for cpu in cpus]

def read(fd, index):
    return struct.unpack("<Q", os.pread(fd, 8, index))[0]

originals = [read(fd, hwcr) for fd in fds]
for fd in fds:
    p0 = read(fd, p0_definition)
    if p0 >> 63 != 1 or (p0 & 0xff) * 200 != 3000 * ((p0 >> 8) & 0x3f):
        raise RuntimeError(f"unexpected P0 definition {p0:#x}")
    if read(fd, pstate_control) & 7 != 0:
        raise RuntimeError("firmware is not set to request P0")
touched = []
try:
    for cpu, fd, original in zip(cpus, fds, originals):
        target = original | cpb_disable
        if target != original:
            touched.append((fd, original))
            os.pwrite(fd, struct.pack("<Q", target), hwcr)
        actual = read(fd, hwcr)
        if actual != target:
            raise RuntimeError(f"CPU {cpu} HWCR readback differs")
        print(f"CPU {cpu} HWCR {original:#018x} -> {actual:#018x}", flush=True)
except Exception:
    for fd, original in touched:
        os.pwrite(fd, struct.pack("<Q", original), hwcr)
    raise
finally:
    for fd in fds:
        os.close(fd)

subprocess.run(["lscpu", "-e=CPU,ONLINE,MHZ,MAXMHZ,MINMHZ"], check=True)
