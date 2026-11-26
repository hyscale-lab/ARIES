package main

import "github.com/hyscale-lab/aries/pkg/deployment/vhive"

const intrastatHealth = `for url in odoo:18080 idev:18082 compliance-hub:18081 services:18083 dms:18084; do curl --fail --silent --show-error --max-time 4 --output /dev/null http://$url/health; done`

var intrastatSetup = taskSetup{output: "/workspace/out/reconciliation.json", packagesSHA: "4cc6f4d8fa12e4877ca8dff05d8e2bd75258c930c73f85b26292e08d2ba48b66", packagesField: "intrastat_wheelhouse_sha256", initialCheck: `printf '\n127.0.0.1 odoo compliance-hub idev services dms\n' >>/etc/hosts
test -d /workspace/out && test ! -e /workspace/out/reconciliation.json
mkdir -p /root/.codex/tmp/arg0/codex-arg03gHF6w
ln -s /root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex /root/.codex/tmp/arg0/codex-arg03gHF6w/apply_patch
test -x /root/.codex/tmp/arg0/codex-arg03gHF6w/apply_patch
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
