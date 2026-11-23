"""Verify matching installed block and DAX runtimes against the captured block arm."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def deployment(runtime, service):
    raw = (runtime / 'runtime.json').read_text()
    paths = ('bin/firecracker', 'bin/firecracker-containerd', 'bin/containerd-shim-aws-firecracker',
             'bin/firecracker-ctr', 'bin/runc', 'bin/jailer', 'common-rootfs.ext4', 'common-vmlinux.bin',
             'common-vmlinux.config')
    files = {name: digest(runtime / name) for name in paths}
    state = subprocess.check_output(['systemctl', 'show', service,
                                     '-p', 'ActiveState', '-p', 'MainPID', '-p', 'ActiveEnterTimestampMonotonic'],
                                    text=True)
    values = dict(line.split('=', 1) for line in state.splitlines())
    executable = Path('/proc') / values['MainPID'] / 'exe'
    assert values['ActiveState'] == 'active'
    assert executable.resolve() == runtime / 'bin/firecracker-containerd'
    assert digest(executable) == files['bin/firecracker-containerd']
    return {
        'files_sha256': files, 'profile_sha256': digest(runtime / 'runtime.json'),
        'normalized_profile': json.loads(raw.replace(str(runtime), '<runtime>')),
        'service': service, 'systemd': values,
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('worker')
    parser.add_argument('runtime_base', type=Path)
    parser.add_argument('service_prefix')
    parser.add_argument('block_reference', type=Path)
    args = parser.parse_args()
    base = args.runtime_base.resolve(strict=True)
    reference = args.block_reference.resolve(strict=True)
    boot_id = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    assert (reference / 'boot-id').read_text().strip() == boot_id
    assert subprocess.run(['pgrep', '-x', 'firecracker|[.]firecracker-re'], capture_output=True).returncode == 1
    profiles = {variant: deployment(base / variant / 'runtime', args.service_prefix + '-' + variant)
                for variant in ('v3', 'v4')}
    prior, candidate = (profiles[v]['normalized_profile'] for v in ('v3', 'v4'))
    assert prior['root_drive_use_pmem'] is False and candidate['root_drive_use_pmem'] is True
    assert dict(prior, root_drive_use_pmem=True) == candidate
    assert prior['guest_reclamation'] == {'Enabled': True} and prior['smt_enabled'] is False
    assert profiles['v3']['files_sha256'] == profiles['v4']['files_sha256']
    assert profiles['v3']['profile_sha256'] == digest(reference / 'runtime.json')
    reference_hashes = {Path(path).name: sha for sha, path in
                        (line.split(None, 1) for line in (reference / 'inputs.sha256').read_text().splitlines())}
    for name in ('bin/firecracker', 'common-vmlinux.bin', 'common-rootfs.ext4'):
        assert reference_hashes[Path(name).name] == profiles['v3']['files_sha256'][name]
    print(json.dumps({'worker': args.worker, 'hostname': os.uname().nodename, 'boot_id': boot_id,
                      'normalized_profile_delta': {'root_drive_use_pmem': [False, True]},
                      'profiles': profiles}, indent=2))


if __name__ == '__main__':
    main()
