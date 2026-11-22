package main

import (
	"context"
	"fmt"
	"maps"

	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

const torchTask = "torch-tensor-parallelism"
const torchWheelCache = "/tmp/aries-torch-wheel-cache"

var torchSetup = taskSetup{
	// The reviewed Torch and SPARQL images have the same Ubuntu package state.
	packagesSHA: "53128b1ff531fb50b8952d571e0af2b5a73b143ae06531f729b172b38e2ced4d",
	output:      "parallel_linear.py",
	initialCheck: `test ! -e parallel_linear.py
test ! -e /tmp/apply_patch
test ! -e /tmp/parallel-linear-deps
test -z "$(command -v python || true)"
test "$(python3 --version)" = 'Python 3.12.3'
command -v curl
python3 -c 'import importlib.util; assert all(importlib.util.find_spec(m) is None for m in ("torch", "pip", "ensurepip"))'`,
}

var torchCPUWheels = map[string]replayWheel{
	"filelock":          {"3.32.3", "7f0ca4bcc0e181c60dbbd8aa9ab5b120ebb99e4e064e83636340056f833a1f09"},
	"fsspec":            {"2026.7.0", "b57ddbafedfaef7018c1ecab32aa200a9d7ca26b77965f64e48b70061249d279"},
	"jinja2":            {"3.1.6", "85ece4451f492d0c13c5dd7c13a64681a86afae63a5f347908daf103ce6d2f67"},
	"markupsafe":        {"3.0.3", "d6dd0be5b5b189d31db7cda48b91d7e0a9795f31430b7f271219ab30f1d3ac9d"},
	"mpmath":            {"1.3.0", "a0b2b9fe80bbcd81a6647ff13108738cfb482d481d826cc0e02f5b35e5c88d2c"},
	"networkx":          {"3.6.1", "d47fbf302e7d9cbbb9e2555a0d267983d2aa476bac30e90dfbe5669bd57f3762"},
	"setuptools":        {"78.1.0", "3e386e96793c8702ae83d17b853fb93d3e09ef82ec62722e61da5cd22376dcd8"},
	"sympy":             {"1.13.1", "db36cdc64bf61b9b24578b6f7bab1ecdd2452cf008f34faa33776680c26d66f8"},
	"typing-extensions": {"4.16.0", "481caa481374e813c1b176ada14e97f1f67a4539ce9cfeb3f350d78d6370c2e8"},
}

type torchTrial struct {
	outputSHA     string
	deployedTorch replayWheel
	noCache       bool
	bootstrapsPip bool
}

var torch251CPU = replayWheel{"2.5.1+cpu", "4856f9d6925121d13c2df07aa7580b767f449dfe71ae5acde9c27535d5da4840"}
var torchBootstrapPip = replayWheel{"26.2.1", "71138adf1f4ca900cdb7d289c21b7494329f2332b6d85f0e1c42108c0384ed3e"}

var torchTrials = map[string]torchTrial{
	"4b588047-22af-4dc2-a705-6b8d24ff88fe": {outputSHA: "74325ccd4e90c129f9214dc46f87a5c269ae2e76effb0b52a651de8f43ea2bb0", deployedTorch: torch251CPU},
	"9765e0a0-be12-40c4-9d2c-99654498a31f": {
		outputSHA:     "08db22206da38a5a10ebfb2edbe809db867571fff2bb8ff084493fd1689057f7",
		deployedTorch: replayWheel{"2.6.0+cpu", "59e78aa0c690f70734e42670036d6b541930b8eabbaa18d94e090abf14cc4d91"},
	},
	"a51a05f5-7fcb-420e-949b-e8cca7666a24": {
		outputSHA: "f79845a8ffb0f71fbd7c2ad2d3a3e67d35da3b9fbb4b6f832a11c012b1500d33", deployedTorch: torch251CPU, noCache: true, bootstrapsPip: true,
	},
	"ce62b3a5-602c-44c7-b13e-6537dca35d1d": {
		outputSHA:     "6e54192205cbe0127948f76682c98f13ef6090fab8f017a67063b6e6af9dc116",
		deployedTorch: replayWheel{"2.13.0+cpu", "4ca4a9394b0c771238a4f73590fdbbc4debad85ed0fa63d026ae1b085da7d6e2"}, noCache: true,
	},
	"e047ddea-bb8c-43c0-b71e-e625a611565f": {outputSHA: "fdea131160e68ca0f48d22baf4b4637b510c8a2ae594e6b05392a21c518865df", deployedTorch: torch251CPU},
}

// These three CPU index URLs omit archive hash fragments. Constrain the
// same wheel URLs with known hashes when the original command disables caching.
var torchUnhashedIndexLinks = map[string]string{
	"jinja2":     "https://download.pytorch.org/whl/jinja2-3.1.6-py3-none-any.whl",
	"markupsafe": "https://download.pytorch.org/whl/markupsafe-3.0.3-cp312-cp312-manylinux2014_x86_64.manylinux_2_17_x86_64.manylinux_2_28_x86_64.whl",
	"setuptools": "https://download.pytorch.org/whl/setuptools-78.1.0-py3-none-any.whl",
}

func (t torchTrial) wheels() map[string]replayWheel {
	wheels := maps.Clone(torchCPUWheels)
	wheels["torch"] = t.deployedTorch
	if t.deployedTorch.Version == "2.13.0+cpu" {
		wheels["sympy"] = replayWheel{"1.14.0", "e091cc3e99d2141a0ba2847328f5479b05d94a6635cb96148ccb3f34671bd8f5"}
	}
	return wheels
}

func prepareTorch(ctx context.Context, m *vhive.Manager, id, trial string) (string, error) {
	spec, ok := torchTrials[trial]
	if !ok {
		return "", fmt.Errorf("no reviewed CPU Torch initial state for %s", trial)
	}
	wheels := spec.wheels()
	if spec.bootstrapsPip {
		wheels["pip"] = torchBootstrapPip
	}
	var links map[string]string
	if spec.noCache {
		links = torchUnhashedIndexLinks
	}
	return preparePipConstraints(ctx, m, id, "torch", wheels, torchWheelCache, links)
}

func torchOutputCheck(trial string) (string, error) {
	spec, ok := torchTrials[trial]
	if !ok {
		return "", fmt.Errorf("no reviewed CPU Torch final state for %s", trial)
	}
	cache := torchWheelCache
	if spec.noCache {
		cache = ""
	}
	report, err := pipReportCheck("torch", spec.wheels(), cache, "download.pytorch.org", "download-r2.pytorch.org", "files.pythonhosted.org")
	if err != nil {
		return "", err
	}
	bootstrap := "\nsha256sum /tmp/parallel-linear-pip.pyz"
	if spec.bootstrapsPip {
		bootstrap = "\nsha256sum /tmp/parallel-linear-get-pip.py\n/tmp/parallel-linear-venv/bin/python - <<'PYBOOTSTRAP'\n" + verifyTorchBootstrap + "\nPYBOOTSTRAP"
	}
	return fmt.Sprintf("\necho '%s  parallel_linear.py' | sha256sum -c -", spec.outputSHA) + bootstrap + report, nil
}

const verifyTorchBootstrap = wheelCacheCheck + `import importlib.metadata, json
url = 'https://files.pythonhosted.org/packages/f3/6e/1736e5b4ae2b778ef2f81c47d797de9f891d4d8acb047a24ca37a60294dd/pip-26.2.1-py3-none-any.whl'
version = importlib.metadata.version('pip')
sha = hash_cached_wheel('/tmp/aries-torch-wheel-cache', url)
assert version == '26.2.1' and sha == '71138adf1f4ca900cdb7d289c21b7494329f2332b6d85f0e1c42108c0384ed3e'
print(json.dumps({'name': 'pip', 'version': version, 'sha256': sha, 'url': url, 'sha256_source': 'retained_http_body'}, sort_keys=True))`

const torchNetworkProbe = `import json, socket, urllib.request
from pathlib import Path
hosts = {
    'bootstrap.pypa.io': 'https://bootstrap.pypa.io/pip/pip.pyz',
    'download.pytorch.org': 'https://download.pytorch.org/whl/cpu/torch/',
    'download-r2.pytorch.org': 'https://download-r2.pytorch.org/whl/cpu/torch-2.6.0%2Bcpu-cp312-cp312-linux_x86_64.whl',
}
checks = {}
for host, url in hosts.items():
    request = urllib.request.Request(url, method='HEAD')
    with urllib.request.urlopen(request, timeout=10) as response:
        checks[host] = {'addresses': sorted({a[4][0] for a in socket.getaddrinfo(host, 443, socket.AF_INET)}),
                        'status': response.status}
print(json.dumps({'route': Path('/proc/net/route').read_text(), 'resolv_conf': Path('/etc/resolv.conf').read_text(),
                  'dependencies': checks}))`
