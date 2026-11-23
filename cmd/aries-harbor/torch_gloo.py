"""Inspect default Gloo networking and a hostname alias on one private guest."""
import fcntl
import ipaddress
import json
import os
from pathlib import Path
import socket
import struct
import sys
import tempfile


HOSTS = Path('/etc/hosts')
SAVED = Path('/tmp/aries-torch-gloo-original-hosts')


def inspect():
    hostname = socket.gethostname()
    interfaces = {}
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as sock:
        for _, name in socket.if_nameindex():
            try:
                entry = fcntl.ioctl(sock.fileno(), 0x8915, struct.pack('256s',name.encode()))  # SIOCGIFADDR
                interfaces[name] = socket.inet_ntoa(entry[20:24])
            except OSError:
                continue
    resolved, bindable = [], []
    error = None
    try:
        resolved = sorted({row[4][0] for row in socket.getaddrinfo(hostname,None,socket.AF_INET,socket.SOCK_STREAM)})
        for addr in resolved:
            try:
                with socket.socket() as sock:
                    sock.bind((addr,0))
                bindable.append(addr)
            except OSError:
                continue
    except socket.gaierror as exc:
        error = str(exc)
    limits = dict(logical_cpus=os.cpu_count(), allowed_cpus=sorted(os.sched_getaffinity(0)))
    if limits != {'logical_cpus':1, 'allowed_cpus':[0]} or os.getenv('GLOO_SOCKET_IFNAME'):
        raise RuntimeError('expected one guest CPU and default Gloo device selection')
    return dict(hostname=hostname, ipv4_interfaces=interfaces, resolved_ipv4=resolved, bindable_ipv4=bindable,
                hostname_lookup_error=error, cpu=limits, gloo_socket_ifname=os.getenv('GLOO_SOCKET_IFNAME'))


def listeners():
    opened = set()
    for fd in Path('/proc/self/fd').iterdir():
        try:
            target = os.readlink(fd)
        except FileNotFoundError:
            continue
        if target.startswith('socket:['):
            opened.add(target.removeprefix('socket:[').removesuffix(']'))
    bound = set()
    for line in Path('/proc/net/tcp').read_text().splitlines()[1:]:
        field = line.split()
        if field[9] in opened and field[3] == '0A':
            address = ipaddress.IPv4Address(bytes.fromhex(field[1].split(':')[0])[::-1])
            bound.add(str(address))
    return bound


def gloo_address(expected, wheel, import_root):
    os.environ['OMP_NUM_THREADS'] = '1'
    if import_root:
        sys.path.insert(0, import_root)
    import torch
    import torch.distributed as dist

    if torch.__version__ != wheel:
        raise RuntimeError(f'unexpected Gloo wheel {torch.__version__}')
    torch.set_num_threads(1)
    before = listeners()
    with tempfile.TemporaryDirectory(prefix='aries-gloo-canary-') as tmp:
        dist.init_process_group('gloo', init_method=f'file://{tmp}/store', rank=0, world_size=1)
        try:
            addresses = sorted(listeners()-before)
            if addresses != [expected]:
                raise RuntimeError(f'unexpected Gloo TCP listener {addresses}; wanted {expected}')
            return dict(torch_version=torch.__version__, default_gloo_tcp_listener=addresses, torch_threads=torch.get_num_threads())
        finally:
            dist.destroy_process_group()


action, wheel, import_root = sys.argv[1:]
before = inspect()
result = dict(before=before)
if action == 'install':
    if before['bindable_ipv4']:
        raise RuntimeError('hostname already resolves to a bindable local address')
    ip = before['ipv4_interfaces']['eth0']
    if ipaddress.ip_address(ip).is_loopback:
        raise RuntimeError('expected a real guest eth0 address')
    original = HOSTS.read_bytes()
    with SAVED.open('xb') as copy:
        copy.write(original)
    HOSTS.write_bytes(original.rstrip(b'\n') + f"\n{ip} {before['hostname']}\n".encode())
if action in ('install','verify','canary'):
    result['alias'] = inspect()
    expected = result['alias']['ipv4_interfaces']['eth0']
    if result['alias']['bindable_ipv4'] != [expected]:
        raise RuntimeError('private hostname alias did not select the guest NIC')
    if action == 'canary':
        result['device'] = gloo_address(expected, wheel, import_root)
if action == 'original-canary':
    HOSTS.write_bytes(SAVED.read_bytes())
    result['original'] = inspect()
    if result['original']['bindable_ipv4']:
        raise RuntimeError('the preserved hosts file unexpectedly resolves the hostname')
    result['device'] = gloo_address('127.0.0.1', wheel, import_root)
print(json.dumps(result,sort_keys=True))
