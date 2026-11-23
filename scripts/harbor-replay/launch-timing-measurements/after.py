"""Hold a native HF successor behind the acknowledged local archive controller."""
import subprocess,time,sys
from pathlib import Path
variant,worker,marker=sys.argv[1:]
while not (Path(marker).is_file() and Path(marker).stat().st_size):
    time.sleep(15)
subprocess.run(['ssh','-o','BatchMode=yes','nehalem@amd'+worker+'.utah.cloudlab.us','sudo','systemd-run','--unit=zb-timing17-hf','/usr/bin/python3','/tmp/zb-timing17-01/hf.py',variant],check=True)
