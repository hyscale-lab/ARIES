package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

const datasetTask = "count-dataset-tokens"
const datasetPythonTrial = "eaa24caa-f211-4745-a943-0f7041aa4214"

// The source printed Python 3.13.15 before creating /tmp/openthoughts-venv.
// The published image provides 3.13.7 with an older ensurepip seed.
const datasetPythonCheck = `test "$(python --version)" = 'Python 3.13.15'
python - <<'PYVERSION'
import ensurepip
from pathlib import Path
import shutil
import sysconfig
base = Path(sysconfig.get_path("purelib"))
old = base / "pip-25.2.dist-info"
if old.exists():
    assert "Version: 25.2" in (old / "METADATA").read_text().splitlines()
    shutil.rmtree(old)
bundles = Path(ensurepip.__file__).parent / "_bundled"
(bundles / "pip-25.2-py3-none-any.whl").unlink(missing_ok=True)
assert sorted(x.name for x in bundles.glob("pip-*.whl")) == ["pip-26.2.1-py3-none-any.whl"]
assert ensurepip.version() == "26.2.1"
assert sorted(x.name for x in base.glob("pip-*.dist-info")) == ["pip-26.2.1.dist-info"]
print("source Python 3.13.15, clean virtual environment pip seed", ensurepip.version())
PYVERSION
`

var datasetSetup = taskSetup{output: "answer.txt", initialCheck: `test ! -e answer.txt
test "$(python -c 'import sys; print(sys.version_info[:2])')" = "(3, 13)"
python - <<'PYBASE'
import importlib.util
for module in ("datasets","transformers"):
    assert importlib.util.find_spec(module) is None, module
PYBASE
cat > /tmp/aries-dataset-requirements.txt <<'PINS'
aiohappyeyeballs==2.7.1
aiohttp==3.14.3
aiosignal==1.4.0
anyio==4.14.2
attrs==26.1.0
certifi==2026.7.22
charset_normalizer==3.5.1
datasets==5.0.1
dill==0.4.1
filelock==3.32.5
frozenlist==1.8.0
fsspec==2026.6.0
h11==0.16.0
hf-xet==1.6.0
httpcore==1.0.9
httpx==0.28.1
huggingface-hub==0.36.2
idna==3.19
multidict==6.7.1
multiprocess==0.70.19
numpy==2.5.2
packaging==26.3
pandas==3.0.5
propcache==0.5.2
pyarrow==25.0.1
python-dateutil==2.9.0.post0
pyyaml==6.0.3
regex==2026.8.31
requests==2.34.2
safetensors==0.8.0
six==1.17.0
tokenizers==0.22.2
tqdm==4.70.0
transformers==4.57.6
typing-extensions==4.16.0
urllib3==2.7.0
xxhash==4.0.1
yarl==1.24.5
PINS
printf '%s\n' 'export PIP_CONSTRAINT=/tmp/aries-dataset-requirements.txt' 'export PIP_REPORT=/tmp/aries-dataset-install.json' > /etc/profile.d/zz-aries-dataset.sh
python -m pip install --no-cache-dir --no-index --no-deps --disable-pip-version-check 'https://files.pythonhosted.org/packages/f3/6e/1736e5b4ae2b778ef2f81c47d797de9f891d4d8acb047a24ca37a60294dd/pip-26.2.1-py3-none-any.whl#sha256=71138adf1f4ca900cdb7d289c21b7494329f2332b6d85f0e1c42108c0384ed3e'
python -m pip --version
sha256sum /tmp/aries-dataset-requirements.txt`}

const datasetNetworkProbe = `import json,socket,urllib.request
from pathlib import Path
sites={"dataset":"https://huggingface.co/api/datasets/ryanmarten/OpenThoughts-1k-sample","tokenizer":"https://huggingface.co/api/models/Qwen/Qwen2.5-1.5B-Instruct"}
expected={"dataset":"a82400884621626d41bef89b7604f8054e7e00e0","tokenizer":"989aa7980e4cf806f80c7fef2b1adb7bc71aa306"}
if use_revision_endpoint:
    sites = {key: url + "/revision/" + expected[key] for key, url in sites.items()}
for key,url in sites.items():
    with urllib.request.urlopen(url,timeout=15) as response:
        assert json.load(response)["sha"] == expected[key], key
hosts=("huggingface.co","us.aws.cdn.hf.co","files.pythonhosted.org","pypi.org")
print(json.dumps({"route":Path("/proc/net/route").read_text(),"resolver":Path("/etc/resolv.conf").read_text(),"checked_revisions":expected,"checked_urls":sites,"resolved_endpoints":{h:sorted({x[4][0] for x in socket.getaddrinfo(h,443)}) for h in hosts}}))`

const datasetFinal = `
python - <<'PYFINAL'
import hashlib,json
from pathlib import Path
answer=Path("/app/answer.txt").read_bytes()
assert answer.strip() == b"79586",answer
print("token total",answer.decode().strip())
for item in json.loads(Path("/tmp/aries-dataset-install.json").read_text())["install"]:
    print(json.dumps({"package":item["metadata"]["name"],"version":item["metadata"]["version"],"wheel":item["download_info"]},sort_keys=True))
PYFINAL`

// Host CNI resolves and TLS-verifies these four service names, and allows each
// selected address on the private bridge. Verify its private DNS view from
// the live guest before using public package or dataset services.
func prepareDatasetEndpoints(ctx context.Context, m *vhive.Manager, id, conf string) error {
	raw, err := os.ReadFile(filepath.Join(conf, "dataset-hosts.json"))
	if err != nil {
		return err
	}
	var addresses map[string]netip.Addr
	if err = json.Unmarshal(raw, &addresses); err != nil {
		return err
	}
	expected := []string{"huggingface.co", "us.aws.cdn.hf.co", "files.pythonhosted.org", "pypi.org"}
	if len(addresses) != len(expected) {
		return errors.New("unexpected dataset endpoint count")
	}

	for _, name := range expected {
		addr := addresses[name]
		if !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
			return fmt.Errorf("invalid dataset endpoint for %s", name)
		}

	}
	_, err = checked(ctx, m, id, core.Command{Path: "/usr/local/bin/python", Args: []string{"-c", `import json,socket,sys
for name, expected in json.loads(sys.argv[1]).items():
    seen = {a[4][0] for a in socket.getaddrinfo(name,443,socket.AF_INET,socket.SOCK_STREAM)}
    assert seen == {expected}, (name, expected, seen)
`, string(raw)}})
	return err
}

// Steps 10 and 15 of this original locate /usr/bin/patch and list the
// transient harness directory; the source does not publish its plugin files.
const datasetPatchDiscoveryTrial = "a3571ce0-b75f-4df8-b0c9-9907d1ff73be"
