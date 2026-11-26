package main

import "github.com/hyscale-lab/aries/pkg/deployment/vhive"

const intrastatHealth = `for url in odoo:18080 idev:18082 compliance-hub:18081 services:18083 dms:18084; do curl --fail --silent --show-error --max-time 4 --output /dev/null http://$url/health; done`

var intrastatSetup = taskSetup{output: "/workspace/out/reconciliation.json", packagesSHA: "4cc6f4d8fa12e4877ca8dff05d8e2bd75258c930c73f85b26292e08d2ba48b66", packagesField: "intrastat_wheelhouse_sha256", initialCheck: `printf '\n127.0.0.1 odoo compliance-hub idev services dms\n' >>/etc/hosts
test -d /workspace/out && test ! -e /workspace/out/reconciliation.json
printf '[global]\nno-index = true\nfind-links = /tmp\n' > /etc/pip.conf
` + intrastatHealth}

func intrastatCompanions() []vhive.CompanionTask {
	return []vhive.CompanionTask{
		{Role: "idev", Image: "docker.io/harborframework/terminal-bench@sha256:b1ec3caa5ca80a7b2a00ec28ec54a51dbc7680ea297578a5bf27ab4ae79949e1", Command: []string{"/usr/bin/env", "RUNTIME_DIR=/var/lib/intrastat-runtime", "python", "/app/intrastat_server.py", "idev"}},
		{Role: "odoo", Image: "docker.io/harborframework/terminal-bench@sha256:5b35fea6469419b2f2fafd9a562cbe9bab0d38decfdcdf27a3c8f8fc4acf9778", Command: []string{"python", "/app/intrastat_server.py", "odoo"}},
		{Role: "services", Image: "docker.io/harborframework/terminal-bench@sha256:e7af68dd2519d09eefc6e68d714e4115ef35f67ca8b674507bd3f93ac0d3f9c0", Command: []string{"python", "/app/intrastat_server.py", "services"}},
		{Role: "dms", Image: "docker.io/harborframework/terminal-bench@sha256:5722eaf160ff537576d713a09e4c3ebd9b200d6b5054e02ab607e434a7dfd082", Command: []string{"/usr/bin/env", "RUNTIME_DIR=/var/lib/intrastat-runtime", "python", "/app/intrastat_server.py", "dms"}},
		{Role: "compliance-hub", Image: "docker.io/harborframework/terminal-bench@sha256:c019cea99f27450679b0390571e6d68640dff055534b0f5c5e798fa30661138e", Command: []string{"/bin/sh", "-ec", "printf '\\n127.0.0.1 idev\\n' >>/etc/hosts; exec \"$@\"", "compliance-hub", "/usr/bin/env", "RUNTIME_DIR=/var/lib/intrastat-runtime", "IDEV_URL=http://idev:18082", "python", "/app/intrastat_server.py", "compliance-hub"}, PrivateHostsFile: true},
	}
}

var intrastatAliases = map[string]string{
	"cfff4007-c001-4e29-950d-eb1a1e8a7bb0": "codex-arg03gHF6w",
	"506d9672-f546-46ed-8232-37ed8009fe96": "codex-arg0EUDB6q",
	"0e67fd64-d0a0-4afb-9afe-17e48f2f0719": "codex-arg0pKUL19",
}

const intrastatAliasSetup = `
mkdir -p /root/.codex/tmp/arg0/ALIAS
ln -s /root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex /root/.codex/tmp/arg0/ALIAS/apply_patch
test -x /root/.codex/tmp/arg0/ALIAS/apply_patch`

const intrastatFirstFinal = `
python - <<'PYINTRA'
import datetime, hashlib, json
from pathlib import Path

root = Path('/workspace/out')
memo = json.loads((root / 'reconciliation.json').read_bytes())
def utc(value):
    stamp = datetime.datetime.fromisoformat(value)
    assert stamp.utcoffset() == datetime.timedelta(0)
    return '<UTC timestamp>'
for ticket in memo['tickets']:
    ticket['ts'] = utc(ticket['ts'])
memo['written_at'] = utc(memo['written_at'])
times = {f['direction']: f['filed_at'] for f in memo['filings']}
for filing in memo['filings']:
    filing['filed_at'] = utc(filing['filed_at'])
for entry in memo['archive_manifest']:
    name = Path(entry['path']).name
    direction = name.split('-', 1)[0]
    assert entry['metadata']['X-DMS-Filed-At'] == times[direction]
    entry['metadata']['X-DMS-Filed-At'] = utc(times[direction])
    body = (root / name).read_bytes()
    assert entry['bytes'] == len(body) and entry['sha256'] == hashlib.sha256(body).hexdigest()
    if name.endswith('-receipt.pdf'):
        assert body.startswith(b'%PDF') and b'%%EOF' in body
    else:
        assert name.endswith('-filed.xml')
    print('live-archive', name, entry['sha256'], len(body))
    del entry['sha256'], entry['bytes']
stable = hashlib.sha256(json.dumps(memo, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
print('reconciliation stable fields', stable)
assert stable == '166fa61ed631d20a3000d7af3f941e72a06698300a61400e8a86c85e11fbc9f0'
print('/workspace/out/reconciliation.json stable fields: OK')
PYINTRA
`

const intrastatLaterFinal = `
python - <<'PYINTRA'
import datetime as dt
import email.utils
import hashlib
import json
import re
from pathlib import Path

root = Path('/workspace/out')
memo = json.loads((root / 'reconciliation.json').read_bytes())
written = dt.datetime.fromisoformat(memo['written_at'])
assert written.utcoffset() == dt.timedelta(0)
records = memo.get('archive_files') or memo.get('archive_artifacts')
if records is None:
    if 'archive' in memo:
        records = memo['archive']['filing_artifacts']
    else:
        records = [r for f in memo['filings'].values() for r in f['artifacts'].values()]
archives = [r for r in records if 'sha256' in r]
assert len(archives) == 4
for r in archives:
    path = r.get('path') or r['archive_path']
    filename = Path(path).name
    assert filename.endswith(('.pdf', '.xml'))
    digest = r['sha256']
    assert re.fullmatch('[a-f0-9]{64}', digest)
    matches = [p for p in root.rglob('*' + Path(path).suffix) if p.is_file() and hashlib.sha256(p.read_bytes()).hexdigest() == digest]
    assert matches, 'no matching live document: ' + filename
    size = matches[0].stat().st_size
    assert r.get('bytes', size) == size
    if 'read_back_headers' in r:
        headers = r['read_back_headers']
        assert int(headers['Content-Length']) == r['bytes']
        stamp = email.utils.parsedate_to_datetime(headers['Date'])
        assert abs((stamp - written).total_seconds()) < 3600
        headers['Date'] = '<HTTP response time>'
        headers['Content-Length'] = '<verified file size>'
        r['put_response'] = json.loads(r['put_response'])
    if 'bytes' in r:
        r['bytes'] = '<verified file size>'
    print('live archive digest', filename, digest)
    r['sha256'] = '<verified live digest>'

stamp_pattern = re.compile(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)')
def stable_stamp(match):
    stamp = dt.datetime.fromisoformat(match[0].replace('Z', '+00:00'))
    # The fixed effective dates and service observations outside this session remain literal.
    return '<session timestamp>' if abs((stamp - written).total_seconds()) < 3600 else match[0]

encoded = json.dumps(memo, sort_keys=True, separators=(',', ':'))
actual = hashlib.sha256(stamp_pattern.sub(stable_stamp, encoded).encode()).hexdigest()
print('reconciliation stable fields', actual)
assert actual == 'EXPECTED_STABLE'
print('/workspace/out/reconciliation.json stable fields: OK')
PYINTRA
`
