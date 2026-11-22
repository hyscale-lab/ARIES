package main

import _ "embed"

const financialTask = "financial-document-processor"

//go:embed financial_inputs.json
var financialInputs string

// The OCI build randomized filenames; preserve content and reproduce the
// original names only after checking every source file against the inventory.
var financialSetup = taskSetup{
	packagesSHA: sparqlSetup.packagesSHA,
	output:      "invoices/summary.csv",
	inputCheck:  ":",
	initialCheck: `test ! -e /app/invoices && test ! -e /app/other
test "$(uv --version)" = 'uv 0.8.14'
python3 - <<'FINANCIAL'
import hashlib, json
from pathlib import Path
import importlib.util
for name in ("pypdf","PIL","fitz","cv2","pytesseract"):
    assert importlib.util.find_spec(name) is None, name
items = json.loads('''` + financialInputs + `''')
root = Path("/app/documents")
assert {p.name for p in root.iterdir()} == {r["published"] for r in items}
for r in items:
    f = root/r["published"]
    assert f.is_file() and not f.is_symlink() and f.stat().st_size == r["bytes"]
    assert hashlib.sha256(f.read_bytes()).hexdigest() == r["sha256"]
    assert not (root/r["recorded"]).exists()
for r in items:
    (root/r["published"]).rename(root/r["recorded"])
config = Path("/root/.config/uv/uv.toml")
config.parent.mkdir(exist_ok=True, parents=True)
with config.open("x") as f:
    f.write('exclude-newer = "2026-08-31T21:28:16Z"\n')
print("17 original-sized, hash-verified files restored under their recorded names")
FINANCIAL`,
}

const financialMuPDFTrial = "3e740f93-fb59-47e8-9033-bcadb10a87dd"

const financialTempAllocation = `
# Only one source uses this prefix and later consumes its recorded path literally.
setup_dir=/usr/local/lib/python3.12/dist-packages
test -d "$setup_dir"
test ! -e /tmp/document-sort-9_0og0zq
cat > "$setup_dir/aries_document_temp.py" <<'TEMP'
import os
import tempfile

_original = tempfile.mkdtemp
def _recorded(*args, **kwargs):
    if not args and kwargs == {"prefix":"document-sort-", "dir":"/tmp"}:
        path = "/tmp/document-sort-9_0og0zq"
        if not os.path.lexists(path):
            os.mkdir(path, 0o700)
            return path
    return _original(*args, **kwargs)
tempfile.mkdtemp = _recorded
TEMP
echo 'import aries_document_temp' > "$setup_dir/aries_document_temp.pth"
python3 -c 'import tempfile; assert tempfile.mkdtemp.__module__ == "aries_document_temp"' `

var financialFinalCheck = `
echo 'b1b30b37c3e038f91ac5e06d5f75e2df0b53f8565305e7b8ba9919e0559bfb00  invoices/summary.csv' | sha256sum -c -
python3 - <<'FINANCIAL'
import hashlib, json
from pathlib import Path
root = Path("/app")
items = json.loads('''` + financialInputs + `''')
assert not any((root/"documents").iterdir())
for r in items:
    paths = [root/folder/r["recorded"] for folder in ("invoices","other")]
    f, = [p for p in paths if p.is_file() and not p.is_symlink()]
    assert hashlib.sha256(f.read_bytes()).hexdigest() == r["sha256"]
print("Verified all 17 document hashes after sorting")
FINANCIAL`

const financialPypdfCheck = `
python3 - <<'FINANCIAL'
import email
from pathlib import Path
metadata = {}
for p in Path("/root/.cache/uv").rglob("*.dist-info/METADATA"):
    m = email.message_from_bytes(p.read_bytes())
    name = m["Name"].lower()
    if name in ("pypdf","pillow"):
        metadata.setdefault(name,set()).add(m["Version"])
assert set(metadata)=={"pypdf","pillow"}, metadata
print(sorted((k,sorted(v)) for k,v in metadata.items()))
FINANCIAL`
