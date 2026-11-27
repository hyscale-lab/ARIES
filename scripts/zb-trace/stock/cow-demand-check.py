"""Bind the active stock configuration and startup-pack audit to each successful file load."""
import argparse
import hashlib
import json
from pathlib import Path
import tomllib


def read_lines(path):
    with path.open() as stream:
        for line in stream:
            try:
                yield json.loads(line)
            except json.JSONDecodeError:
                if line.lstrip().startswith('{'):
                    raise


def audit(service, config, gate):
    pid = int((service / 'server.pid').read_text())
    args = Path(f'/proc/{pid}/cmdline').read_bytes().split(b'\0')
    assert args[args.index(b'--config') + 1].decode() == str(config.resolve())
    cfg = tomllib.loads(config.read_text())
    pack = cfg['snapshot']['memory_startup_pack']
    assert pack['enabled'] is False and pack['consume_enabled'] is False
    assert cfg['memory_snapshot']['background_download']['enable'] is False
    store = service / 'home/snapshot-store/repository'
    restores = []
    for row in read_lines(gate):
        if row['event'] != 'load_reply':
            continue
        load = row['load']
        assert row['status'] == 204 and load['resume_vm'] is False and load['track_dirty_pages'] is True
        assert load['mem_backend']['backend_type'] == 'File'
        path = Path(load['snapshot_path'])
        assert path.parents[1] == store / 'snapshots'
        snapshot = path.parent.name
        record = json.loads((store / 'catalog/records' / (snapshot + '.json')).read_text())
        assert record['id'] == snapshot
        assert not any(record['committed'].get(k) for k in ('memory_startup', 'memoryStartup'))
        assert not (path.parent / 'memory-startup.pack').exists()
        restores.append(dict(pid=row['pid'], load_reply_ns=row['reply_ns'], snapshot=snapshot,
                             memory_backend=load['mem_backend'], catalog_startup_manifest=None))
    assert restores
    logs = list(read_lines(service / 'server.log'))
    messages = [r['fields'].get('message', '') for r in logs if 'fields' in r and
                ('startup_pack' in r.get('target', '') or 'startup pack' in r['fields'].get('message', '').lower()
                 or 'startup prefetch' in r['fields'].get('message', '').lower())]
    assert not messages, messages
    return dict(server_pid=pid, effective_config=str(config), config_sha256=hashlib.sha256(config.read_bytes()).hexdigest(),
                startup_pack=pack, background_download=False, restores=restores,
                stock_startup_pack_log_events=messages)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ['service', 'config', 'gate']:
        parser.add_argument(key, type=Path)
    args = parser.parse_args()
    print(json.dumps(audit(args.service, args.config, args.gate), indent=2))
