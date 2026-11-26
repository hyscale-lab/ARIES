"""Check the two explicitly named, CPU-partitioned AgentENV services on this host."""
import argparse
import json
import os
from pathlib import Path
import re
import socket
import struct
import subprocess
import sys
import tomllib

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from idle import start_identity


def check(ok, reason):
    if not ok:
        raise ValueError(reason)


def effective_cpus(pid):
    entry, = [r[3:] for r in (Path('/proc') / str(pid) / 'cgroup').read_text().splitlines() if r.startswith('0::')]
    text = (Path('/sys/fs/cgroup') / entry.lstrip('/') / 'cpuset.cpus.effective').read_text().strip()
    result = set()
    for field in text.split(','):
        first, _, last = field.partition('-')
        result.update(range(int(first), int(last or first) + 1))
    check(os.sched_getaffinity(pid) <= result, 'task CPUs exceed effective cgroup partition')
    return result


def socket_owner(path):
    # Reading peer credentials avoids queuing an API request behind a snapshot.
    parent = os.open(path.parent, os.O_DIRECTORY)
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
            client.settimeout(2)
            client.connect(f'/proc/self/fd/{parent}/{path.name}')
            return struct.unpack('3i', client.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[0]
    finally:
        os.close(parent)


def service(unit):
    check(re.fullmatch(r'fcmod-zepbound-[a-z0-9-]+', unit), 'invalid service name')
    pid = int(subprocess.check_output(['systemctl', 'show', '-p', 'MainPID', '--value', unit]))
    proc = Path('/proc') / str(pid)
    argv = proc.joinpath('cmdline').read_bytes().rstrip(b'\0').decode().split('\0')
    check(len(argv) == 3 and Path(argv[0]).name == 'firecracker-containerd' and argv[1] == '--config',
          f'{unit} is not a dedicated Firecracker service')
    config = Path(argv[2]).resolve(strict=True)
    rt = config.parent
    cfg = tomllib.loads(config.read_text())
    api_address = f'/run/{unit}/containerd.sock'
    plugins = cfg['plugins']
    check(plugins['io.containerd.server.v1.grpc']['address'] == api_address and
          Path(api_address).is_socket() and plugins['io.containerd.snapshotter.v1.devmapper']['pool_name'] == unit,
          f'{unit} socket or thin pool differs')
    check(socket_owner(Path(api_address)) == pid, f'{unit} containerd socket peer differs from its systemd MainPID')
    profile = json.loads((rt / 'runtime.json').read_bytes())
    check(profile['guest_reclamation_profile'] == 'agentenv' and profile['smt_enabled'] is False and
          Path(profile['shim_base_dir']).samefile(rt / 'shim'), f'{unit} profile or shim differs')
    return dict(unit=unit, runtime=rt, root=Path(profile['root_drive']),
                root_sha256=profile['root_drive_sha256'], cpus=effective_cpus(pid))


def partitions(current, peer, launcher):
    check(current and peer and current.isdisjoint(peer) and launcher and launcher <= current,
          'service CPU partitions overlap or launcher is outside its partition')


def verify_vm_inventory(live, owners):
    check(live == set(owners), 'foreign or unverified VMM on the worker')


def inventory():
    live = set()
    for proc in Path('/proc').glob('[0-9]*/comm'):
        try:
            comm = proc.read_text().strip()
            if comm == 'firecracker' or comm.startswith('qemu-system'):
                pid = int(proc.parent.name)
                live.add((pid, start_identity(pid)))
        except (FileNotFoundError, ProcessLookupError):
            continue
    return live


def inspect(unit, runtime, peer, require_pair=False, restored_api=None):
    current, other = service(unit), service(peer)
    check(unit != peer and current['runtime'].samefile(runtime), 'unexpected local runtime')
    partitions(current['cpus'], other['cpus'], effective_cpus(os.getpid()))
    for res in ('', 'checkpoints', 'shim', 'devmapper', '.tb41.lock'):
        left, right = (cfg['runtime'] / res for cfg in (current, other))
        check(left.resolve() != right.resolve() and not (left.exists() and right.exists() and left.samefile(right)),
              f'shared runtime resource {res}')
    check(current['root_sha256'] == other['root_sha256'] and not current['root'].samefile(other['root']),
          'colocated roots must match bytes and have independent inodes')
    live = inventory()
    matched_api = False
    owners = {}
    for cfg in (current, other):
        sockets = list(cfg['runtime'].glob('shim/aries#*/firecracker.sock'))
        check(len(sockets) <= 1, f'multiple guest sockets in {cfg["unit"]}')
        for path in sockets:
            pid = socket_owner(path)
            start = start_identity(pid)
            proc = Path('/proc') / str(pid)
            argv = proc.joinpath('cmdline').read_bytes().rstrip(b'\0').decode().split('\0')
            vm_id = path.parent.name.removeprefix('aries#')
            check('--id' in argv and argv[argv.index('--id') + 1] == vm_id and
                  proc.joinpath('comm').read_text().strip() == 'firecracker', 'guest socket and VM identity differ')
            if restored_api and cfg['unit'] == unit and path.samefile(restored_api):
                matched_api = True
            tids = list((Path('/proc') / str(pid) / 'task').iterdir())
            check(effective_cpus(pid) <= cfg['cpus'], 'VMM moved outside service partition')
            check(tids and all(os.sched_getaffinity(int(t.name)) <= cfg['cpus'] for t in tids),
                  f'{cfg["unit"]} VMM thread escaped its assigned CPUs')
            check(start_identity(pid) == start, 'VMM changed during placement check')
            owners[pid, start] = dict(unit=cfg['unit'], vm_id=vm_id, pid=pid, start_ticks=start)
    verify_vm_inventory(live, owners)
    if restored_api:
        check(matched_api, 'sampler socket does not identify the restored local VMM')
    elif require_pair:
        check(len(owners) == 2, 'both colocated guests must be live')
    else:
        check(all(o['unit'] == peer for o in owners.values()), 'local service already has a guest')
    return dict(service=unit, peer=peer, cpus=sorted(current['cpus']), peer_cpus=sorted(other['cpus']),
                vmms=list(owners.values()))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('unit')
    parser.add_argument('runtime', type=Path)
    parser.add_argument('peer')
    parser.add_argument('--require-pair', action='store_true')
    parser.add_argument('--restored-api', type=Path)
    args = parser.parse_args()
    print(json.dumps(inspect(args.unit, args.runtime, args.peer, args.require_pair, args.restored_api)))
