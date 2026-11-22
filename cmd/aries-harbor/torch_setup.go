package main

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
	"torch":             {"2.6.0+cpu", "59e78aa0c690f70734e42670036d6b541930b8eabbaa18d94e090abf14cc4d91"},
	"typing-extensions": {"4.16.0", "481caa481374e813c1b176ada14e97f1f67a4539ce9cfeb3f350d78d6370c2e8"},
}

func torchOutputCheck() (string, error) {
	report, err := pipReportCheck("torch", torchCPUWheels, torchWheelCache, "download.pytorch.org", "download-r2.pytorch.org", "files.pythonhosted.org")
	if err != nil {
		return "", err
	}
	return `
echo '08db22206da38a5a10ebfb2edbe809db867571fff2bb8ff084493fd1689057f7  parallel_linear.py' | sha256sum -c -
sha256sum /tmp/parallel-linear-pip.pyz` + report, nil
}

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
