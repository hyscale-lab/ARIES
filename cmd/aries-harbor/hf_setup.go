package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed hf_trials.json
var hfProfilesJSON []byte

type hfSource struct {
	Dependency string            `json:"dependency"`
	Files      map[string]string `json:"files"`
}

var hfTrials = func() map[string]hfSource {
	var profiles map[string]hfSource
	if err := json.Unmarshal(hfProfilesJSON, &profiles); err != nil {
		panic(err)
	}
	return profiles
}()

const hfGunicornLoopbackTrial = "249f2934-972f-4c61-af3d-0315ce5003c0"

func hfResolvesHostnameToLoopback(trial string) bool {
	return trial == hfLoopbackTrial || trial == hfDefaultThreadsTrial || trial == hfGunicornLoopbackTrial
}

const hfLoopbackTrial = "f0d5e4b5-b20d-40bf-89ec-6483b65525dc"
const hfTask = "hf-model-inference"
const hfEnvironmentSHA = "04ac35b54101efa80a45737dac838f3e51dfbd481f079be1495ffdfe4f05732a"

var hfSetup = taskSetup{
	output: "app.py",
	initialCheck: `test ! -e /app/app.py && test ! -e /app/model_cache
test "$(python --version)" = 'Python 3.13.15'
# Overlaying the official interpreter leaves the published image’s old pip metadata.
python - <<'PIPBASE'
from email.parser import Parser
from pathlib import Path
import shutil
old = Path("/usr/local/lib/python3.13/site-packages/pip-25.3.dist-info")
if old.exists():
    m = Parser().parsestr((old/"METADATA").read_text())
    assert m["Name"] == "pip" and m["Version"] == "25.3"
    shutil.rmtree(old)
PIPBASE
python -m pip install --no-index --no-deps --force-reinstall /tmp/aries-hf-setup/*.whl
python - <<'PYCHECK'
import json, re
from pathlib import Path
from importlib.metadata import distributions
manifest = json.loads(Path("/tmp/aries-hf-setup/wheels.json").read_text())
norm = lambda key: re.sub("[-_.]+", "-", key).lower()
expected = {norm(w["name"]): w["version"] for w in manifest["wheels"]}
expected.update(manifest["image_package"])
installed = [(norm(d.metadata["Name"]), d.version) for d in distributions()]
actual = dict(installed)
assert len(actual) == len(installed), installed
assert actual == expected, (actual.items() - expected.items(), expected.items() - actual.items())
print(json.dumps(actual, sort_keys=True))
PYCHECK
rm -r /tmp/aries-hf-setup
`,
}

func hfSourceSetup(trial string) string {
	extra := ""
	// Original HTTP tests resolve the sandbox hostname to an IPv4 loopback address.
	if hfResolvesHostnameToLoopback(trial) {
		extra = `
python - <<'HOSTCHECK'
from pathlib import Path
import socket
name = socket.gethostname()
assert name and not any(c.isspace() or c == "#" for c in name)
try:
    socket.gethostbyname(name)
except socket.gaierror:
    with Path("/etc/hosts").open("a") as hosts:
        hosts.write("\n127.0.0.1 localhost " + name + "\n")
assert socket.gethostbyname(name) == "127.0.0.1"
print("Verified original sandbox hostname resolves to loopback")
HOSTCHECK
`
	}
	if trial == "071ce48d-3de4-4c02-9e42-3eb64933acd2" {
		extra = `
alias_dir=/root/.codex/tmp/arg0/codex-arg03HR69E
target=/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex
test -x "$target"
mkdir -p "$alias_dir"
ln -s "$target" "$alias_dir/apply_patch"
`
	}
	if dep := hfTrials[trial].Dependency; dep != "" {
		extra += fmt.Sprintf(`
test ! -e /etc/pip.conf
echo '%s' > /etc/aries-hf-constraints.txt
printf '[install]\nconstraint = /etc/aries-hf-constraints.txt\n' > /etc/pip.conf`, dep)
	}
	return extra
}

func hfFinalCheck(trial string) string {
	profile := hfTrials[trial]
	var cmd strings.Builder
	cmd.WriteString("\nsha256sum -c <<'HASHES'\n")
	for _, name := range []string{"app.py", "download_model.py", "requirements.txt", "README.md"} {
		if digest, ok := profile.Files[name]; ok {
			fmt.Fprintf(&cmd, "%s  %s\n", digest, name)
		}
	}
	cmd.WriteString(`7c3919835e442510166d267fe7cbe847e0c51cd26d9ba07b89a57b952b49b8aa  model_cache/sentiment_model/model.safetensors
HASHES
`)
	if name, version, ok := strings.Cut(profile.Dependency, "=="); ok {
		fmt.Fprintf(&cmd, "python -c 'from importlib.metadata import version; assert version(%q) == %q'\n", name, version)
	}
	cmd.WriteString(`python - <<'PYRESULT'
import requests
r = requests.post("http://127.0.0.1:5000/sentiment", json={"text":"I absolutely love this!"}, timeout=10)
r.raise_for_status()
assert r.json()["sentiment"] == "positive"
print("Guest sentiment service returned positive")
PYRESULT`)
	return cmd.String()
}

const hfNetworkProbe = `import json
from urllib.request import urlopen
urls = ["https://pypi.org/simple/waitress/", "https://huggingface.co/api/models/distilbert/distilbert-base-uncased-finetuned-sst-2-english"]
with urlopen(urls[0], timeout=12) as pip, urlopen(urls[1], timeout=12) as model:
    revision = json.load(model)["sha"]
    assert revision == "714eb0fa89d2f80546fda750413ed43d93601a13", revision
    print(json.dumps({"pypi_status":pip.status,"model_revision":revision}))`
