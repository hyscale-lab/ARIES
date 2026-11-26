package main

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

const hofTopologyTask = "hof-topology-interpenetration"
const hofSystreTrial = "27c84b6a-af43-4332-9ba4-0f70ab1e160d"
const hofExperimentalTrial = "bb35c2e1-18a6-4dfb-abf7-872812037a8c"
const hofTopologyOfflineTrial = "aabc93fe-c1d8-4e28-af4b-e4e9bcee0540"

// The pinned wheel set is a dated reconstruction. It preserves in-session
// installation and download operations, using a local wheel directory.
var hofTopologyOfflineSetup = taskSetup{
	packagesSHA:   "8eaed8c085e2de8faa2175d6a3d4c970e7b6d2cc295654023a5eb781e3659585",
	packagesField: "hof_wheels_archive_sha256",
	output:        "/app/solution/output.json",
	inputCheck: `export LC_ALL=C
test "$(sha256sum data/*.cif | sha256sum)" = '05d79a5106dc248f3104f254013c6fb220a7cd5465c9904ae29af59e07b071ca  -'`,
	initialCheck: `test ! -e /app/solution
if command -v apply_patch || command -v java || command -v unzip; then exit 1; fi
test ! -e /etc/profile.d/zz-aries-hof-venv.sh
printf '%s\n' 'export PATH="/opt/venv/bin:$PATH"' > /etc/profile.d/zz-aries-hof-venv.sh
. /etc/profile.d/zz-aries-hof-venv.sh
test "$(command -v python)" = /opt/venv/bin/python
python - <<'ARIESPY'
import importlib.util, sys
assert sys.version_info[:2] == (3, 12)
assert all(importlib.util.find_spec(name) is None for name in (
    'numpy', 'scipy', 'networkx', 'gemmi', 'sympy', 'mpmath', 'jdk4py', 'mofstructure',
))
ARIESPY
pip --version
(cd /opt/zb-hof/wheels && sha256sum -c ../SHA256SUMS)
test ! -e /etc/pip.conf
cat > /etc/pip.conf <<'ARIESPIP'
[global]
no-index = true
find-links = file:///opt/zb-hof/wheels
ARIESPIP
sha256sum /etc/pip.conf
alias_dir=/root/.codex/tmp/arg0/codex-arg02OZDT5
target=/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex
test -x "$target"
mkdir -p "$alias_dir"
ln -s "$target" "$alias_dir/apply_patch"
readlink "$alias_dir/apply_patch"
if command -v apply_patch; then exit 1; fi`,
}

func stageHOFWheels(ctx context.Context, m *vhive.Manager, id, archive, digest string) error {
	const wheelDir = "/opt/zb-hof"
	if _, err := checked(ctx, m, id, core.Command{Path: "/usr/bin/install", Args: []string{"-d", wheelDir}}); err != nil {
		return err
	}
	return uploadPinned(ctx, m, id, archive, digest, wheelDir)
}
