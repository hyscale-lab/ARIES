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

func preparePipConstraints(ctx context.Context, m *vhive.Manager, id, tag string, wheels map[string]replayWheel, cacheDir string, links map[string]string) (string, error) {
	constraints := make([]string, 0, len(wheels))
	for name, wheel := range wheels {
		constraint := name + "==" + wheel.Version
		if url, ok := links[name]; ok {
			constraint = name + " @ " + url + "#sha256=" + wheel.SHA256
		}
		constraints = append(constraints, constraint)
	}
	sort.Strings(constraints)
	cacheCheck, cacheOption := "", ""
	if cacheDir != "" {
		cacheCheck = "test ! -e " + cacheDir + "\n"
		cacheOption = "cache-dir = " + cacheDir + "\n"
	}
	setup := "set -eu\n" + cacheCheck + `
test ! -e /etc/pip.conf
test ! -e /tmp/aries-PIPTAG-pip-report.json
cat > /etc/aries-PIPTAG-constraints.txt <<'ARIESCONSTRAINTS'
` + strings.Join(constraints, "\n") + `
ARIESCONSTRAINTS
cat > /etc/pip.conf <<'ARIESPIP'
[install]
constraint = /etc/aries-PIPTAG-constraints.txt
report = /tmp/aries-PIPTAG-pip-report.json
` + cacheOption + `ARIESPIP
sha256sum /etc/pip.conf /etc/aries-PIPTAG-constraints.txt`
	return checked(ctx, m, id, core.Command{Path: "/bin/sh", Args: []string{"-c", strings.ReplaceAll(setup, "PIPTAG", tag)}})
}

func pipReportCheck(tag string, installed map[string]replayWheel, cacheDir string, hosts ...string) (string, error) {
	items, err := json.Marshal(installed)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("\npython3 - '/tmp/aries-%s-pip-report.json' '%s' '%s' '%s' <<'PYREPORT'\n%s\nPYREPORT", tag, items, strings.Join(hosts, ","), cacheDir, checkPipReport), nil
}

const wheelCacheCheck = `import hashlib
from pathlib import Path

def hash_cached_wheel(cache_dir, url):
    # CacheControl stores this URL's original HTTP body under its SHA-224 key.
    key = hashlib.sha224(url.encode()).hexdigest()
    path = (Path(cache_dir) / 'http-v2').joinpath(*key[:5], key + '.body')
    with path.open('rb') as cached:
        return hashlib.file_digest(cached, 'sha256').hexdigest()
`

const checkPipReport = wheelCacheCheck + `import json, sys, urllib.parse
expected = json.loads(sys.argv[2])
hosts = set(sys.argv[3].split(','))
cache_dir = sys.argv[4]
installed = {}
for item in json.loads(Path(sys.argv[1]).read_text())['install']:
    meta, download = item['metadata'], item['download_info']
    name = meta['name'].lower().replace('_', '-')
    url = urllib.parse.urlparse(download['url'])
    if name in installed or url.scheme != 'https' or url.hostname not in hosts:
        raise ValueError(f'unexpected wheel {name} from {download["url"]}')
    sha = download['archive_info'].get('hashes', {}).get('sha256')
    evidence = {}
    if not sha:
        if not cache_dir:
            raise ValueError(f'no pip report digest for {name}')
        sha = hash_cached_wheel(cache_dir, download['url'])
        evidence['sha256_source'] = 'retained_http_body'
    installed[name] = {'version': meta['version'], 'sha256': sha}
    print(json.dumps({'name': name, **installed[name], 'url': download['url'], **evidence}, sort_keys=True))
if installed != expected:
    raise ValueError(f'installed wheels differ from the checked source selections: {installed}')`
