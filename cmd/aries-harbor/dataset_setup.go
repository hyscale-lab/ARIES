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
for key,url in sites.items():
    with urllib.request.urlopen(url,timeout=15) as response:
        assert json.load(response)["sha"] == expected[key], key
hosts=("huggingface.co","us.aws.cdn.hf.co","files.pythonhosted.org","pypi.org")
print(json.dumps({"route":Path("/proc/net/route").read_text(),"resolver":Path("/etc/resolv.conf").read_text(),"checked_revisions":expected,"resolved_endpoints":{h:sorted({x[4][0] for x in socket.getaddrinfo(h,443)}) for h in hosts}}))`

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
// selected address on the private bridge. Pin the same endpoints in the guest
// so regional DNS differences cannot select an address outside its scoped rules.
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
	entries := ""
	for _, name := range expected {
		addr := addresses[name]
		if !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsPrivate() {
			return fmt.Errorf("invalid dataset endpoint for %s", name)
		}
		entries += addr.String() + " " + name + "\n"
	}
	_, err = checked(ctx, m, id, core.Command{Path: "/usr/local/bin/python", Args: []string{"-c", `import sys
with open("/etc/hosts","a") as f: f.write("\n"+sys.argv[1])
`, entries}})
	return err
}
