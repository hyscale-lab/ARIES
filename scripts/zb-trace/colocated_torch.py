"""Run the sealed 4b588047 source twice per backing period with Git memory counters."""
import argparse
import concurrent.futures
from functools import partial
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import threading
import time
import tomllib

from colocated_git import host_sample
from vmm_exclusivity import processes


TRIAL = '4b588047-22af-4dc2-a705-6b8d24ff88fe'
TASK = 'torch-tensor-parallelism'
INVENTORY = '6f50c5bd8fa791b90e600d487842668baf38e1e1835d68c3cc55426fa9dd06f2'
ROOT_SHA = '292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f'
PERIODS = ('v3', 'v4', 'v4', 'v3')
SOLO_PERIODS = ('v4', 'v3')
EVENTS = ('create_start', 'snapshot_start', 'restore_complete', 'session_start', 'session_end', 'removed')
GIB = 2**30


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def identity(path):
    path = path.resolve(strict=True)
    info = path.stat()
    row = dict(path=str(path), device=info.st_dev, inode=info.st_ino, size=info.st_size)
    if stat.S_ISBLK(info.st_mode):
        row['block_rdev'] = info.st_rdev
    return row


def open_backing(proc, configured):
    configured = Path(configured)
    alias = proc/'root'/str(configured).lstrip('/') if configured.is_absolute() else proc/'cwd'/configured
    # The API path is relative to Firecracker's private restore root. Its
    # effective mount may expose a different inode from the worker pathname.
    aliases = {os.path.normpath(str(configured)), os.path.normpath(str(alias.resolve(strict=False)))}
    aliases.add(os.path.normpath(str(Path(os.readlink(proc/'root'))/str(configured).lstrip('/'))))
    opened = []
    for descriptor in (proc/'fd').iterdir():
        try:
            target = os.readlink(descriptor)
            if os.path.normpath(target.removesuffix(' (deleted)')) not in aliases:
                continue
            opened.append((descriptor.name, target, descriptor.stat()))
        except FileNotFoundError:
            continue
    if opened:
        if len({(s.st_dev,s.st_ino) for _,_,s in opened}) != 1:
            raise ValueError(f'ambiguous open backing for {configured}')
        _, target, info = opened[0]
        return dict(configured_path=str(configured), opened_path=target, device=info.st_dev, inode=info.st_ino,
                    size=info.st_size, block_rdev=info.st_rdev if stat.S_ISBLK(info.st_mode) else None,
                    evidence='open-fd', descriptors=[n for n,_,_ in opened])
    for line in (proc/'maps').read_text().splitlines():
        fields = line.split(None,5)
        if len(fields) == 6 and os.path.normpath(fields[-1].removesuffix(' (deleted)')) in aliases:
            backing = proc/'map_files'/fields[0]
            info = backing.stat()
            return dict(configured_path=str(configured), opened_path=fields[-1], device=info.st_dev,
                        inode=info.st_ino,size=info.st_size,block_rdev=None,evidence='mapped-file',mapping=fields[0])
    raise ValueError(f'active VMM has no observable backing for {configured}')


def measured_root(runtime):
    profile = json.loads((runtime/'runtime.json').read_text())
    expected = runtime/'shim/.pmem-images'/(ROOT_SHA+'.img')
    if digest(expected) != ROOT_SHA:
        raise ValueError('installed shared root inode does not match the sealed root bytes')
    return profile, identity(expected)


def preflight(assets, runtimes, units, output):
    if processes():
        raise RuntimeError('another VMM is running on the worker')
    if digest(assets/'SHA256SUMS') != INVENTORY:
        raise ValueError('unexpected sealed source bundle')
    subprocess.run(['sha256sum', '--quiet', '-c', 'SHA256SUMS'], cwd=assets, check=True)
    cni = assets/'cni/conf/10-torch.conflist'
    net = json.loads(cni.read_text())['plugins'][0]['ipam']
    subnet = ipaddress.ip_network(net['subnet'])
    leased = []
    for path in (Path(net['dataDir'])/'zbtorch201').glob('*'):
        try:
            address = ipaddress.ip_address(path.name)
        except ValueError:
            continue
        if address in subnet:
            leased.append(str(address))
    if leased or subnet.num_addresses < 8:
        raise ValueError(f'Torch CNI scope is occupied or too small: {leased}')
    disk = os.statvfs(output.parent)
    free_disk = disk.f_bavail*disk.f_frsize
    groups, roots, configurations = {}, {}, {}
    for variant, runtime in runtimes.items():
        service = units[variant]
        group = subprocess.check_output(['systemctl','show','-p','ControlGroup','--value',service], text=True).strip()
        if not group.startswith('/system.slice/'):
            raise ValueError('expected dedicated runtime service cgroup')
        groups[variant] = Path('/sys/fs/cgroup')/group.lstrip('/')
        profile, roots[variant] = measured_root(runtime)
        expected = variant == 'v4'
        if (profile['root_drive_use_pmem'] != expected or profile['smt_enabled']
                or not profile['guest_reclamation']['Enabled']):
            raise ValueError(f'unexpected runtime profile {variant}')
        thin = tomllib.loads((runtime/'containerd.toml').read_text())['plugins']['io.containerd.snapshotter.v1.devmapper']
        status = subprocess.check_output(['dmsetup','status',thin['pool_name']], text=True).split()
        used, total = map(int, status[5].split('/'))
        table = subprocess.check_output(['dmsetup','table',thin['pool_name']], text=True).split()
        free_pool = (total-used)*int(table[5])*512
        if int(thin['base_image_size']) != 10*GIB or free_pool < 20*GIB:
            raise ValueError(f'insufficient thin pool capacity {variant}: {free_pool} bytes free')
        configurations[variant] = dict(profile_sha256=digest(runtime/'runtime.json'), pool_free_bytes=free_pool,
                                       shared_root=roots[variant], service=service)
    memory = host_sample(groups)['host_bytes']
    if free_disk < 80*GIB or memory['MemAvailable'] < 32*GIB:
        raise ValueError('worker lacks disk or RAM for two 8 GiB guests and their fresh snapshots')
    receipt = dict(boot_id=Path('/proc/sys/kernel/random/boot_id').read_text().strip(),
                   hostname=os.uname().nodename, filesystem_available_bytes=free_disk,
                   host_available_bytes=memory['MemAvailable'], swap_total_bytes=memory['SwapTotal'],
                   cni_subnet=str(subnet), cni_sha256=digest(cni), preexisting_cni_leases=leased,
                   inventory_sha256=INVENTORY, configurations=configurations,
                   baseline_s=20, launch_stagger_s=40, tail_s=15)
    return groups, roots, receipt


def observe_host(stop, path, groups):
    with path.open('x') as stream:
        while not stop.is_set():
            start = time.monotonic()
            row = host_sample(groups)
            row['vmms'] = processes()
            print(json.dumps(row), file=stream, flush=True)
            stop.wait(max(0, 1-(time.monotonic()-start)))


def live_vm(output, runtime, variant, expected):
    with (output/'events.jsonl').open() as stream:
        first = json.loads(stream.readline())
    with (output/'samples.jsonl').open() as stream:
        sample = json.loads(stream.readline())
    if first['event'] != 'create_start' or not first['runtime_id'].startswith('aries-'+TASK+'-'):
        raise ValueError('unexpected restored VM source identity')
    proc = Path('/proc')/str(sample['pid'])
    task_start = int((proc/'stat').read_text().rsplit(')',1)[1].split()[19])
    if task_start != sample['start_ticks']:
        raise ValueError('reused VMM PID during root readback')
    shim = runtime/'shim'/('aries#'+first['runtime_id'])
    api = subprocess.check_output(['curl','-fsS','--max-time','3','--unix-socket','firecracker.sock',
                                   'http://localhost/vm/config'], cwd=shim)
    cfg = json.loads(api)
    shape = cfg['machine-config']
    if (shape['vcpu_count'],shape['mem_size_mib'],shape['smt']) != (1,8192,False):
        raise ValueError('restored guest shape differs from the sealed source')
    if variant == 'v4':
        root, = cfg['pmem']
        if not (root['id']=='root_pmem' and root['read_only'] and root['root_device']):
            raise ValueError('DAX guest lacks the expected read-only root device')
    else:
        root, = [d for d in cfg['drives'] if d['is_root_device']]
        if not root['is_read_only'] or cfg['pmem']:
            raise ValueError('block guest lacks the expected read-only root drive')
    if root['path_on_host'] != expected['path']:
        raise ValueError('restored VM uses an unexpected configured root input')
    actual = open_backing(proc,root['path_on_host'])
    if actual['size'] != expected['size']:
        raise ValueError('effective root size differs from the verified immutable input')
    writable = [open_backing(proc,d['path_on_host']) for d in cfg['drives'] if not d['is_read_only']]
    if len(writable) < 2:
        raise ValueError('guest workload disk and overlay identities are missing')
    memory = open_backing(proc,shim/'native-restore/memory')
    return dict(runtime_id=first['runtime_id'], pid=sample['pid'], start_ticks=task_start,
                root_input=expected, root=actual, writable=writable, memory=memory,
                netns=os.readlink(proc/'ns/net'), shape=shape,
                socket_inode=(shim/'firecracker.sock').stat().st_ino)


def run_vm(assets, receipt, cli, runtime, unit, variant, root, output, replay_args, verify):
    output.mkdir(mode=0o700)
    folder = assets/'sources'/TASK/TRIAL
    io, checkpoint = (output/name for name in ('io','checkpoint'))
    io.mkdir(mode=0o700)
    checkpoint.mkdir(mode=0o700)
    args = [cli,'-trajectory',folder/'trajectory.json','-rollout',folder/'rollout.jsonl','-image',receipt['image'],
            '-native-checkpoint','-namespace','aries','-socket','/run/'+unit+'/containerd.sock',
            '-checkpoint-dir',checkpoint,'-shim-dir',runtime/'shim','-io-dir',io,
            '-codex-tools',assets/'codex-harbor-tools.tar.gz','-sampler',assets/'tracer/sample_vmm.py',
            '-samples',output/'samples.jsonl','-log',output/'events.jsonl', *replay_args]
    env = dict(os.environ)
    env.pop('ZB_CHECKPOINT_NAMESPACE_DIR', None)
    lineage = None
    with (output/'stderr').open('x') as err, subprocess.Popen(args, stderr=err, stdout=subprocess.DEVNULL, env=env) as proc:
        while proc.poll() is None:
            samples = output/'samples.jsonl'
            if samples.exists():
                with samples.open() as stream:
                    if stream.readline().endswith('\n'):
                        lineage = live_vm(output,runtime,variant,root)
                        (output/'restored-backing.json').write_text(json.dumps(lineage,indent=2)+'\n')
                        break
            time.sleep(.1)
        rc = proc.wait()
    (output/'driver.exit').write_text(f'{rc}\n')
    if rc:
        raise RuntimeError(f'original replay exited {rc} in {output.name}')
    if lineage is None:
        raise ValueError('restored VMM identity was unavailable')
    fidelity = verify(output/'events.jsonl',receipt)
    events = [json.loads(line) for line in (output/'events.jsonl').read_text().splitlines()]
    stamps = {row['event']:row['monotonic_ns'] for row in events if row['event'] in EVENTS}
    return stamps, dict(root_readback=lineage, checkpoint_parent=str(checkpoint), io=str(io), fidelity=fidelity)


def independent(roles):
    for identity in ('runtime_id','pid','netns','socket_inode'):
        first, second = [row['root_readback'][identity] for row in roles]
        if first == second:
            raise ValueError(f'colocated restores collide on {identity}')
    for name in ('checkpoint_parent','io'):
        if roles[0][name] == roles[1][name]:
            raise ValueError(f'colocated restores share writable {name}')
    drives = []
    for row in roles:
        drives.append({('block',d['block_rdev']) if d['block_rdev'] is not None else ('file',d['device'],d['inode'])
                      for d in row['root_readback']['writable']})
    if drives[0] & drives[1]:
        raise ValueError('restored guests share an effective writable backing')
    first, second = [r['root_readback'] for r in roles]
    if (first['memory']['device'],first['memory']['inode']) == (second['memory']['device'],second['memory']['inode']):
        raise ValueError('restored guests share a checkpoint memory inode')
    if first['root_input'] != second['root_input']:
        raise ValueError('colocated restores have different immutable root inputs')
    return (first['root']['device'],first['root']['inode']) == (second['root']['device'],second['root']['inode'])


def run_period(index, variant, output, launch, groups, roots, count):
    if processes():
        raise RuntimeError('worker has another running VMM')
    case = output/f'{index:02d}-{variant}-{count}vm'
    case.mkdir()
    stop = threading.Event()
    with concurrent.futures.ThreadPoolExecutor(3) as pool:
        sampler = pool.submit(observe_host,stop,case/'host.jsonl',groups)
        try:
            time.sleep(20)
            futures = [pool.submit(launch,variant,roots[variant],case/'a')]
            if count == 2:
                time.sleep(40)
                if futures[0].done():
                    futures[0].result()
                    raise ValueError('first replay ended before the second launch')
                futures.append(pool.submit(launch,variant,roots[variant],case/'b'))
            sessions, roles = zip(*(future.result() for future in futures),strict=True)
            shared_root_inode = independent(roles) if count == 2 else None
            time.sleep(15)
        finally:
            stop.set()
        sampler.result()
    if processes():
        raise RuntimeError('worker has a live VMM after the period')
    host = [json.loads(line) for line in (case/'host.jsonl').read_text().splitlines()]
    lo = max(s['session_start'] for s in sessions)
    hi = min(s['session_end'] for s in sessions)
    pids = {(v['root_readback']['pid'],v['root_readback']['start_ticks']) for v in roles}
    overlap = [row for row in host if lo<=row['begin_ns']<=row['end_ns']<=hi]
    if len(overlap)<2 or max(len(h['vmms']) for h in host)!=count or any(
            {(p['pid'],p['start_ticks']) for p in row['vmms']}!=pids for row in overlap):
        raise RuntimeError('worker-wide samples do not establish the intended exclusive VMM placement')
    config = dict(variant=variant, source=TRIAL, warmup=count==1,
                  period_type='solo_reference' if count==1 else 'colocated', session_events=sessions, roles=roles,
                  shared_effective_root_inode=shared_root_inode,
                  worker_frames=len(host), worker_joint_frames=len(overlap), worker_max_vmms=count,
                  service_cgroups={key:str(value) for key,value in groups.items()})
    (case/'result.json').write_text(json.dumps(config,indent=2)+'\n')
    print(case.name, 'verified', flush=True)


def main():
    cli = argparse.ArgumentParser(description=__doc__)
    for name in ('assets','runtime_base','output'):
        cli.add_argument(name,type=Path)
    cli.add_argument('v3_unit')
    cli.add_argument('v4_unit')
    cli.add_argument('--solo',action='store_true',help='Run the two unpaused single-VM references')
    a = cli.parse_args()
    assets, output = a.assets.resolve(strict=True), a.output.resolve()
    if output.exists():
        raise FileExistsError(output)
    units = dict(v3=a.v3_unit,v4=a.v4_unit)
    runtimes = {v:a.runtime_base.resolve(strict=True)/v/'runtime' for v in units}
    groups, roots, hardware = preflight(assets,runtimes,units,output)
    sys.path.insert(0,str(assets))
    from cohort_sparql import replay_args, verify
    entry, = json.loads((assets/'manifest.json').read_text())
    receipt = entry['receipt']
    if entry['trial_id'] != TRIAL or (receipt['vcpus'],receipt['memory_mib'],receipt['storage_mib']) != (1,8192,10240):
        raise ValueError('sealed source trial or resource envelope changed')
    binary = assets/'adapters'/receipt['adapter_binary_sha256']
    if digest(binary) != receipt['adapter_binary_sha256']:
        raise ValueError('sealed adapter digest mismatch')
    network = replay_args(assets,assets/'cni',TASK)
    output.mkdir(mode=0o700)
    order = SOLO_PERIODS if a.solo else PERIODS
    count = 1 if a.solo else 2
    hardware.update(adapter_sha256=digest(binary),order=list(order),guests_per_period=count)
    (output/'preflight.json').write_text(json.dumps(hardware,indent=2)+'\n')
    for index,variant in enumerate(order):
        launch = partial(run_vm,assets,receipt,binary,runtimes[variant],units[variant],
                         replay_args=network,verify=verify)
        run_period(index,variant,output,launch,groups,roots,count)


if __name__ == '__main__':
    main()
