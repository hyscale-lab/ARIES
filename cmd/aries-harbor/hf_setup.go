package main

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
alias_dir=/root/.codex/tmp/arg0/codex-arg03HR69E
target=/root/.nvm/versions/node/v22.23.2/lib/node_modules/@openai/codex/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex
test -x "$target"
mkdir -p "$alias_dir"
ln -s "$target" "$alias_dir/apply_patch"
test ! -e /etc/pip.conf
printf 'waitress==3.0.2\n' > /etc/aries-hf-constraints.txt
printf '[install]\nconstraint = /etc/aries-hf-constraints.txt\n' > /etc/pip.conf`,
}

const hfFinal = `
sha256sum -c <<'HASHES'
aee590a9eefebdebb9c8166ba7ee722b743a82e8ac289e39d8b687d0a015fac0  app.py
b0de23bb30f5162315062ff2c82e7e6c2c521954bc8c2d0407ca76efbad8a2b8  download_model.py
9eb2120ed6b52f02b93d89fa9983bdd310514721a3397b7720a54ed06a4532f9  requirements.txt
671f70aca56327d6791fc29f204f0cdb3fa458cd6198138af099ef48ea9cb31b  README.md
7c3919835e442510166d267fe7cbe847e0c51cd26d9ba07b89a57b952b49b8aa  model_cache/sentiment_model/model.safetensors
HASHES
python - <<'PYRESULT'
from importlib.metadata import version
import requests
assert version("waitress") == "3.0.2"
r = requests.post("http://127.0.0.1:5000/sentiment", json={"text":"I absolutely love this!"}, timeout=10)
r.raise_for_status()
assert r.json()["sentiment"] == "positive"
print("Guest sentiment service returned positive")
PYRESULT`

const hfNetworkProbe = `import json
from urllib.request import urlopen
urls = ["https://pypi.org/simple/waitress/", "https://huggingface.co/api/models/distilbert/distilbert-base-uncased-finetuned-sst-2-english"]
with urlopen(urls[0], timeout=12) as pip, urlopen(urls[1], timeout=12) as model:
    revision = json.load(model)["sha"]
    assert revision == "714eb0fa89d2f80546fda750413ed43d93601a13", revision
    print(json.dumps({"pypi_status":pip.status,"model_revision":revision}))`
