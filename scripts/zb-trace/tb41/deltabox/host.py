"""Run the Roy DeltaBox pilot and timestamp serial events and QEMU PSS."""
import hashlib
import json
import subprocess
import sys
import threading
import time
from pathlib import Path

root = Path(sys.argv[1]).resolve(strict=True)
kernel = root/'linux/vmlinux'
assert hashlib.sha256(kernel.read_bytes()).hexdigest() == '0ba5e9ee744bcb2bac3391e967cf36cfeb10fd3add067055321cb136cd5004c0'
layer = root/'images/roy-direct.qcow2'
chain = json.loads(subprocess.check_output(['qemu-img', 'info', '--backing-chain', '--output=json', layer]))
assert len(chain) == 2 and Path(chain[1]['filename']).resolve() == (root/'images/DeltaBox-ATC2026-v3-l1.qcow2').resolve()
cmd = ['qemu-system-x86_64', '-machine', 'microvm,accel=kvm,acpi=off', '-cpu', 'host',
       '-m', '4096M', '-smp', '2', '-kernel', str(kernel), '-append',
       'console=ttyS0 loglevel=3 root=/dev/vda1 ro init=/usr/local/lib/deltabox-direct/init panic=-1 direct.mode=check',
       '-blockdev', json.dumps({'driver':'qcow2','node-name':'appliance','read-only':True,'file':{'driver':'file','filename':str(layer)}}),
       '-device', 'virtio-blk-device,drive=appliance',
       '-blockdev', json.dumps({'driver':'raw','node-name':'roy','read-only':True,'file':{'driver':'file','filename':str(root/'roy-root.ext4')}}),
       '-device', 'virtio-blk-device,drive=roy', '-nodefaults', '-no-reboot', '-display', 'none', '-monitor', 'none', '-serial', 'stdio', '-nic', 'none']
proc = subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
lock = threading.Lock()
def emit(**row):
    with lock:
        print(json.dumps(dict(host_monotonic_ns=time.monotonic_ns(), **row)), flush=True)
def sample():
    while proc.poll() is None:
        try:
            lines = Path(f'/proc/{proc.pid}/smaps_rollup').read_text().splitlines()
            pss = next(int(line.split()[1]) for line in lines if line.startswith('Pss:'))
            emit(event='pss', pss_kib=pss)
        except (OSError, StopIteration):
            emit(event='pss_unavailable')
        time.sleep(1)
threading.Thread(target=sample, daemon=True).start()
def timeout():
    time.sleep(600)
    if proc.poll() is None:
        emit(event='timeout')
        proc.kill()
threading.Thread(target=timeout, daemon=True).start()
exit_code = None
try:
    for raw in proc.stdout:
        text = raw.decode('utf-8', errors='replace').strip()
        if 'ZB_EVENT ' in text:
            emit(event='guest', payload=json.loads(text.split('ZB_EVENT ', 1)[1]))
        else:
            emit(event='serial', line=text)
        if text.startswith('DELTABOX_DIRECT_EXIT='):
            exit_code = int(text.split('=',1)[1]); break
finally:
    proc.terminate()
    try:
        proc.wait(timeout=8)
    except subprocess.TimeoutExpired:
        proc.kill(); proc.wait()
assert exit_code == 0, f'guest did not succeed: {exit_code}'
