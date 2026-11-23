package main

import (
	_ "embed"
	"strings"
	"time"
)

// The OCI task image supplies Ubuntu and a two-core nproc wrapper.
const compilerBootstrapSHA = "60565372f4d989dfc5238524c4487f90f01f949b1e94175c794d403f281ee7c5"

var compilerSetup = taskSetup{
	output: "/tmp/CompCert/ccomp",
	initialCheck: `test ! -e /tmp/CompCert
test ! -e /tmp/coq-8.16.1
test "$(nproc)" = 2
command -v gcc make git curl
dpkg-query -W gcc make git curl ca-certificates`,
}

// The source reported hits on all four signed suites. Its install transaction
// added the OCaml build dependencies while build-essential was already present.
func compilerAPT(start time.Time) string {
	signed := financialAptSetup(financialAptProfile{Snapshot: start.UTC().Format("20060102T150405Z")})
	signed = strings.Replace(signed, "Suites: noble noble-updates noble-security",
		"Suites: noble noble-updates noble-security noble-backports", 1)
	return signed + `
apt-get -o Acquire::https::CaInfo=/tmp/aries-compiler-ca.pem update -qq
DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::https::CaInfo=/tmp/aries-compiler-ca.pem install -y --no-install-recommends build-essential=12.10ubuntu1 ca-certificates=20260601~24.04.1 curl git procps python3 >/tmp/aries-compiler-ready-apt.log
rm /tmp/aries-compiler-ca.pem
test ! -e /usr/bin/time
test ! -e /usr/bin/coqc && test ! -e /usr/bin/ocamlc && test ! -e /usr/bin/opam
test "$(dpkg-query -W -f='${Version}' build-essential)" = 12.10ubuntu1
test "$(dpkg-query -W -f='${Version}' ca-certificates)" = '20260601~24.04.1'
apt-cache policy libncurses6 bubblewrap build-essential ca-certificates
`
}

const compilerDirectResult = `test "$(git -C /tmp/CompCert rev-parse HEAD)" = 44d67d81b2a9ed5731d2bbfdf56d591ff3046ab5
test "$(git -C /tmp/coq-8.16.1 rev-parse HEAD)" = b35c06c3ab3ed4911311b4a9428a749658d3eff1
cd /tmp/CompCert
git describe --tags --exact-match
git diff --exit-code
cmp ccomp bin/ccomp
test -s driver/Compiler.vo
./bin/ccomp -version
dpkg-query -W ocaml ocaml-findlib menhir libfindlib-ocaml-dev
rg -c ': passed' test.log`

const compilerNetworkProbe = `import json,socket,urllib.request
from pathlib import Path
sites = {"github.com":"https://github.com/AbsInt/CompCert.git/info/refs?service=git-upload-pack",
         "snapshot.ubuntu.com":"https://snapshot.ubuntu.com/ubuntu/SNAPSHOT/dists/noble/InRelease"}
results = {}
for host, url in sites.items():
    with urllib.request.urlopen(urllib.request.Request(url,method="GET"),timeout=15) as response:
        results[host] = {"addresses":sorted({a[4][0] for a in socket.getaddrinfo(host,80 if url.startswith("http:") else 443)}),"status":response.status,"bytes_read":len(response.read(32))}
print(json.dumps({"route":Path("/proc/net/route").read_text(),"resolv_conf":Path("/etc/resolv.conf").read_text(),"dependency_readback":results}))`

const compilerDirectTrial = "d116000a-0e9f-42a4-945e-71b0a65f12a4"

// OPAM commands and installation roots come from each original source record.
func compilerResult(trial string) string {
	if trial == compilerDirectTrial {
		return compilerDirectResult
	}
	root, switchName, installed := "/tmp/compcert-opam", "compcert-3.13.1", "/tmp/CompCert"
	switch trial {
	case "1d11a94f-d715-44d6-9ff6-1f097a8793f8":
		switchName = "compcert"
	case "bf3c1acf-2d7d-4fa4-9b03-fb8edfce1666":
		root, switchName, installed = "/root/.opam", "compcert", "/tmp/CompCert/_install"
	}
	return strings.NewReplacer("OPAM_ROOT", root, "SWITCH_NAME", switchName, "INSTALL_DIR", installed).Replace(`set -eu
test "$(git -C /tmp/CompCert rev-parse HEAD)" = 44d67d81b2a9ed5731d2bbfdf56d591ff3046ab5
cd /tmp/CompCert
git diff --exit-code
test "$(git describe --tags --exact-match)" = v3.13.1
test -s driver/Compiler.vo
test -s driver/Complements.vo
test -x INSTALL_DIR/bin/ccomp
sha256sum ccomp INSTALL_DIR/bin/ccomp
env -i PATH=/usr/bin:/bin /tmp/CompCert/ccomp -version
export OPAMROOT=OPAM_ROOT OPAMROOTISOK=1
opam list --switch=SWITCH_NAME --installed --columns=name,version
find "$OPAMROOT/download-cache" -type f -exec sha256sum {} + | sort`)
}

//go:embed compiler_opam_releases.json
var opamReleases string

func compilerNetworkCheck(trial string, start time.Time) string {
	base := strings.Replace(compilerNetworkProbe, "SNAPSHOT", start.UTC().Format("20060102T150405Z"), 1)
	if trial == compilerDirectTrial {
		return base
	}
	// Observe release metadata and the artifact checksum from the official OPAM
	// repository, alongside the distro and CompCert endpoints used by the guest.
	extra := `
import hashlib
catalog = ` + opamReleases + `
package_metadata = {}
for record in catalog:
    with urllib.request.urlopen(record["metadata_url"],timeout=15) as response:
        metadata=response.read()
    algorithm, expected=record["content_digest"].split(":",1)
    assert (algorithm+"="+expected).encode() in metadata,record["package"]
    package_metadata[record["package"]]={"metadata_sha256":hashlib.sha256(metadata).hexdigest(),"release_digest":record["content_digest"]}
`
	s := strings.Replace(base, "results = {}", extra+"\nresults = {}", 1)
	return strings.Replace(s, `"dependency_readback":results`, `"dependency_readback":results,"opam_releases":package_metadata`, 1)
}
