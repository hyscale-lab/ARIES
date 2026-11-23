"""Retrieve, validate and publish content-free PyTorch colocation observations."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile

from colocated_torch import EVENTS, INVENTORY, PERIODS, TRIAL, independent


def digest(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream,'sha256').hexdigest()


def remote(host, command, **kwargs):
    return subprocess.run(['ssh','-o','BatchMode=yes',host,command],check=True,**kwargs)


def collect(host, path, raw):
    if not re.fullmatch(r'/local/[a-zA-Z0-9/_-]+', path):
        raise ValueError('expected a private diagnostic path on the worker')
    archive = raw/'worker.tar.gz'
    if archive.exists():
        raise FileExistsError(archive)
    status = remote(host, 'cat '+shlex.quote(path+'.exit'), capture_output=True,text=True).stdout.strip()
    raw.mkdir(parents=True,exist_ok=True)
    (raw/'driver.exit').write_text(status+'\n')
    with (raw/'worker.log').open('xb') as out:
        remote(host,'cat '+shlex.quote(path+'.log'),stdout=out)
    select = '''
set -euo pipefail
cd "$1"
shopt -s nullglob
files=(preflight.json [0-9][0-9]-*/result.json [0-9][0-9]-*/host.jsonl)
for f in events.jsonl samples.jsonl stderr driver.exit restored-backing.json; do
    files+=([0-9][0-9]-*/[ab]/"$f")
done
nice -n 10 ionice -c 3 tar -czf - -- "${files[@]}"
'''
    with archive.with_suffix('.partial').open('xb') as out:
        remote(host,'sudo -n bash -s -- '+shlex.quote(path),input=select.encode(),stdout=out)
    archive.with_suffix('.partial').replace(archive)
    return archive, status


def verify_case(case, closure, verify):
    results = json.loads((case/'result.json').read_text())
    if results['source'] != TRIAL or results['warmup']:
        raise ValueError('expected the original two-VM PyTorch trial')
    roles = results['roles']
    if independent(roles)!=results['shared_effective_root_inode']:
        raise ValueError('reported sharing differs from the actual backing identities')
    for name, role, stamps in zip(('a','b'),roles,results['session_events'],strict=True):
        directory = case/name
        log = directory/'events.jsonl'
        actual = verify(log,closure)
        if actual != role['fidelity'] or (directory/'driver.exit').read_text().strip() != '0':
            raise ValueError('replay fidelity does not match the sealed source')
        events = [json.loads(line) for line in log.read_text().splitlines()]
        sample_rows = [json.loads(line) for line in (directory/'samples.jsonl').read_text().splitlines()]
        who = role['root_readback']
        if json.loads((directory/'restored-backing.json').read_text()) != who:
            raise ValueError('captured live backing evidence differs from the period receipt')
        if {(s['pid'],s['start_ticks']) for s in sample_rows} != {(who['pid'],who['start_ticks'])}:
            raise ValueError('PSS sample identity differs from live socket readback')
        if events[0]['runtime_id'] != who['runtime_id'] or stamps != {
                row['event']:row['monotonic_ns'] for row in events if row['event'] in EVENTS}:
            raise ValueError('replay identity or clock does not match the host receipt')
        role['raw_events_sha256'] = digest(log)
        role['pss_samples_sha256'] = digest(directory/'samples.jsonl')
        role['fidelity'] = {k:v for k,v in actual.items() if k!='final_checks'}
        role['fidelity']['final_checks_sha256'] = hashlib.sha256(actual['final_checks'].encode()).hexdigest()
    host = [json.loads(line) for line in (case/'host.jsonl').read_text().splitlines()]
    begin = max(row['session_start'] for row in results['session_events'])
    end = min(row['session_end'] for row in results['session_events'])
    expected = {(r['root_readback']['pid'],r['root_readback']['start_ticks']) for r in roles}
    joint = [row for row in host if begin<=row['begin_ns']<=row['end_ns']<=end]
    if (len(host)!=results['worker_frames'] or len(joint)!=results['worker_joint_frames']
            or max(len(row['vmms']) for row in host)!=2 or any(
                {(p['pid'],p['start_ticks']) for p in row['vmms']}!=expected for row in joint)):
        raise ValueError('host samples do not corroborate the measured two-VM overlap')
    results['host_samples_sha256'] = digest(case/'host.jsonl')
    results['status'] = 'diagnostic'
    return results


def publish(archive, assets, target):
    if digest(assets/'SHA256SUMS') != INVENTORY:
        raise ValueError('sealed bundle revision differs')
    subprocess.run(['sha256sum','--quiet','-c','SHA256SUMS'],cwd=assets,check=True)
    sys.path.insert(0,str(assets))
    from cohort_sparql import verify
    entry, = json.loads((assets/'manifest.json').read_text())
    closure = entry['receipt']
    target.parent.mkdir(parents=True,exist_ok=True)
    if target.exists():
        raise FileExistsError(target)
    with tempfile.TemporaryDirectory(dir=target.parent,prefix='torch-colocation-') as temp:
        staging = Path(temp)/'raw'
        staging.mkdir()
        with tarfile.open(archive) as stream:
            stream.extractall(staging,filter='data')
        publication = Path(temp)/'results'
        publication.mkdir()
        preflight = json.loads((staging/'preflight.json').read_text())
        if preflight['inventory_sha256'] != INVENTORY or preflight['adapter_sha256'] != closure['adapter_binary_sha256']:
            raise ValueError('measured source bundle lineage differs')
        shutil.copy(staging/'preflight.json',publication)
        for index, variant in enumerate(PERIODS):
            name = f'{index:02d}-{variant}-2vm'
            raw = staging/name
            observations = publication/name
            meta = verify_case(raw,closure,verify)
            if meta['variant'] != variant or any(r['root_readback']['root_input'] !=
                    preflight['configurations'][variant]['shared_root'] for r in meta['roles']):
                raise ValueError('period backing or shared root inode differs')
            observations.mkdir()
            shutil.copy(raw/'host.jsonl',observations)
            for role in ('a','b'):
                (observations/role).mkdir()
                shutil.copy(raw/role/'samples.jsonl',observations/role)
            meta.update(node='amd201',raw_archive_sha256=digest(archive))
            (observations/'result.json').write_text(json.dumps(meta,indent=2)+'\n')
        publication.rename(target)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('host')
    p.add_argument('worker_path')
    for name in ('raw','assets','target'):
        p.add_argument(name,type=Path)
    p.add_argument('--reuse-archive',action='store_true',help='Verify previously collected raw inputs')
    args = p.parse_args()
    if args.reuse_archive:
        archive = args.raw/'worker.tar.gz'
        status = (args.raw/'driver.exit').read_text().strip()
    else:
        archive, status = collect(args.host,args.worker_path,args.raw.resolve())
    if status != '0':
        raise SystemExit(f'campaign exit {status}; raw worker observations retained')
    publish(archive,args.assets.resolve(strict=True),args.target.resolve())


if __name__ == '__main__':
    main()
