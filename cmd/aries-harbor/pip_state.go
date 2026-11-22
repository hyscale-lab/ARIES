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

type replayWheel struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

func preparePipConstraints(ctx context.Context, m *vhive.Manager, id, tag string, wheels map[string]replayWheel) (string, error) {
	constraints := make([]string, 0, len(wheels))
	for name, wheel := range wheels {
		constraints = append(constraints, name+"=="+wheel.Version)
	}
	sort.Strings(constraints)
	setup := `set -eu
test ! -e /etc/pip.conf
test ! -e /tmp/aries-PIPTAG-pip-report.json
cat > /etc/aries-PIPTAG-constraints.txt <<'ARIESCONSTRAINTS'
` + strings.Join(constraints, "\n") + `
ARIESCONSTRAINTS
cat > /etc/pip.conf <<'ARIESPIP'
[install]
constraint = /etc/aries-PIPTAG-constraints.txt
report = /tmp/aries-PIPTAG-pip-report.json
ARIESPIP
sha256sum /etc/pip.conf /etc/aries-PIPTAG-constraints.txt`
	return checked(ctx, m, id, core.Command{Path: "/bin/sh", Args: []string{"-c", strings.ReplaceAll(setup, "PIPTAG", tag)}})
}

func pipReportCheck(tag string, installed map[string]replayWheel, hosts ...string) (string, error) {
	items, err := json.Marshal(installed)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("\npython3 - '/tmp/aries-%s-pip-report.json' '%s' '%s' <<'PYREPORT'\n%s\nPYREPORT", tag, items, strings.Join(hosts, ","), checkPipReport), nil
}

const checkPipReport = `import json, sys, urllib.parse
from pathlib import Path
expected = json.loads(sys.argv[2])
hosts = set(sys.argv[3].split(','))
installed = {}
for item in json.loads(Path(sys.argv[1]).read_text())['install']:
    meta, download = item['metadata'], item['download_info']
    name = meta['name'].lower().replace('_', '-')
    url = urllib.parse.urlparse(download['url'])
    if name in installed or url.scheme != 'https' or url.hostname not in hosts:
        raise ValueError(f'unexpected wheel {name} from {download["url"]}')
    installed[name] = {'version': meta['version'], 'sha256': download['archive_info']['hashes']['sha256']}
    print(json.dumps({'name': name, **installed[name], 'url': download['url']}, sort_keys=True))
if installed != expected:
    raise ValueError(f'installed wheels differ from the checked source selections: {installed}')`
