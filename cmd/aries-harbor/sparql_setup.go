package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

const sparqlTask = "sparql-university"

var sparqlSetup = taskSetup{
	packagesSHA: "53128b1ff531fb50b8952d571e0af2b5a73b143ae06531f729b172b38e2ced4d",
	output:      "solution.sparql",
	inputCheck:  `echo '0b0b1f2c0ee7ea7ddc9382f94ff8181996ffa7be60a1651111c1f57b49e09a85  university_graph.ttl' | sha256sum -c -`,
	initialCheck: `test ! -e solution.sparql
command -v curl wget unzip
python3 -c 'import importlib.util; assert importlib.util.find_spec("rdflib") is None'
python3 --version
curl --version | head -1`,
}

type sparqlWheel struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

var sparqlPyparsing = sparqlWheel{"3.3.2", "850ba148bd908d7e2411587e247a1e4f0327839c40e2e5e6d05a007ecc69911d"}

var sparqlRDFLib = map[string]sparqlWheel{
	"rdflib":    {"7.1.4", "72f4adb1990fa5241abd22ddaf36d7cafa5d91d9ff2ba13f3086d339b213d997"},
	"pyparsing": sparqlPyparsing,
}

var sparqlRDFLibAndPydantic = map[string]sparqlWheel{
	"annotated-types":   {"0.8.0", "f072f4d804ea359e4eaf198b1af7a8b0943881a87f31bb764f8bf219bb9419e0"},
	"pydantic":          {"2.13.5", "346a034f080da3755d8e9cb5e00e8b07de1d39e4f6e2c87d8ab7cafa0b269a73"},
	"pydantic-core":     {"2.46.5", "0fc5be0abd4a407e200d844b404e33639a554e7bd0d448e7b9ae181be4789ac2"},
	"pyparsing":         sparqlPyparsing,
	"rdflib":            {"7.6.0", "30c0a3ebf4c0e09215f066be7246794b6492e054e782d7ac2a34c9f70a15e0dd"},
	"typing-extensions": {"4.16.0", "481caa481374e813c1b176ada14e97f1f67a4539ce9cfeb3f350d78d6370c2e8"},
	"typing-inspection": {"0.4.4", "65b8397ba37ccbce054456aaccddfc91e6e3083c92824df348d96ca832f3f147"},
}

type sparqlTrialState struct {
	querySHA  string
	installed map[string]sparqlWheel
	downloads []string
}

var sparqlTrialStates = map[string]sparqlTrialState{
	"11f94696-6ae2-414f-9d90-e5bf611cf364": {querySHA: "55298834d26a419149cb9d464a96789aa35f8f7aeb226fe6c1dd10756e8fae1a"},
	"28b314c3-2ea7-47d2-983b-9ea71befe578": {
		querySHA: "d917897981ecdbab4b747791021c853ac5aed8a070554484bbc49b6835c3840c", installed: sparqlRDFLib,
		downloads: []string{"/tmp/university-pip.pyz"},
	},
	"786300a5-e689-4b20-ada9-edd20999f11e": {
		querySHA: "236a5228e0bf864874178cdbdf422c732c2f53ec7e272f45ea918e304a4a8e88", installed: sparqlRDFLib,
		downloads: []string{"/tmp/university-sparql-validation.7tKi8V/pip.pyz"},
	},
	"ec088b8c-b4e4-49e4-8107-6228c0690a21": {
		querySHA: "b091912c92cc2335bf144eb8d3f68e29f29db47881ffc3fca9fce1e25f54cb62", installed: sparqlRDFLib,
		downloads: []string{"/tmp/university-query-pip.pyz"},
	},
	"f2d28757-76b8-423d-8f9b-e64b5fb61aa7": {
		querySHA: "811e0e56250e898ed4a7210fb4d3f762828238e232fbc9e6221b2c6906f7de2c", installed: sparqlRDFLibAndPydantic,
		downloads: []string{"/tmp/sparql-tools.zyCf5e/pip.pyz", "/tmp/sparql-tools.zyCf5e/apply_patch.py"},
	},
}

func prepareSparql(ctx context.Context, m *vhive.Manager, id, trial string) (string, error) {
	state, ok := sparqlTrialStates[trial]
	if !ok {
		return "", fmt.Errorf("no reviewed SPARQL initial state for %s", trial)
	}
	if len(state.installed) == 0 {
		return "", nil
	}
	constraints := make([]string, 0, len(state.installed))
	for name, wheel := range state.installed {
		constraints = append(constraints, name+"=="+wheel.Version)
	}
	sort.Strings(constraints)
	setup := `set -eu
test ! -e /etc/pip.conf
test ! -e /tmp/aries-sparql-pip-report.json
cat > /etc/aries-sparql-constraints.txt <<'ARIESCONSTRAINTS'
` + strings.Join(constraints, "\n") + `
ARIESCONSTRAINTS
cat > /etc/pip.conf <<'ARIESPIP'
[install]
constraint = /etc/aries-sparql-constraints.txt
report = /tmp/aries-sparql-pip-report.json
ARIESPIP
sha256sum /etc/pip.conf /etc/aries-sparql-constraints.txt`
	return checked(ctx, m, id, core.Command{Path: "/bin/sh", Args: []string{"-c", setup}})
}

func sparqlOutputCheck(trial string) (string, error) {
	state, ok := sparqlTrialStates[trial]
	if !ok {
		return "", fmt.Errorf("no reviewed SPARQL final state for %s", trial)
	}
	check := fmt.Sprintf("\nprintf '%%s\\n' '%s  solution.sparql' | sha256sum -c -", state.querySHA)
	if len(state.installed) == 0 {
		return check, nil
	}
	items, err := json.Marshal(state.installed)
	if err != nil {
		return "", err
	}
	return check + "\nsha256sum " + strings.Join(state.downloads, " ") + "\npython3 - '" + string(items) + "' <<'PYREPORT'\n" + sparqlReportCheck + "\nPYREPORT", nil
}

const sparqlReportCheck = `import json, sys, urllib.parse
from pathlib import Path
expected = json.loads(sys.argv[1])
installed = {}
for item in json.loads(Path('/tmp/aries-sparql-pip-report.json').read_text())['install']:
    meta, download = item['metadata'], item['download_info']
    name = meta['name'].lower().replace('_', '-')
    url = urllib.parse.urlparse(download['url'])
    if name in installed or url.scheme != 'https' or url.hostname != 'files.pythonhosted.org':
        raise ValueError(f'unexpected wheel {name} from {download["url"]}')
    installed[name] = {'version': meta['version'], 'sha256': download['archive_info']['hashes']['sha256']}
    print(json.dumps({'name': name, **installed[name], 'url': download['url']}, sort_keys=True))
if installed != expected:
    raise ValueError(f'installed wheels differ from the checked source selections: {installed}')`

const sparqlNetworkProbe = `import json, socket, urllib.request
from pathlib import Path
hosts = {'bootstrap.pypa.io': 'https://bootstrap.pypa.io/pip/pip.pyz', 'pypi.org': 'https://pypi.org/simple/rdflib/'}
checks = {}
for host, url in hosts.items():
    request = urllib.request.Request(url, method='HEAD')
    with urllib.request.urlopen(request, timeout=10) as response:
        checks[host] = {'addresses': sorted({a[4][0] for a in socket.getaddrinfo(host, 443, socket.AF_INET)}),
                        'status': response.status}
print(json.dumps({'route': Path('/proc/net/route').read_text(), 'resolv_conf': Path('/etc/resolv.conf').read_text(),
                  'dependencies': checks}))`
