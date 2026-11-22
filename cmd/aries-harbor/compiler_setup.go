package main

// The published image contains Ubuntu and the task's two-core nproc wrapper.
// The original first step additionally observes GCC, make, Git, and curl.
var compilerSetup = taskSetup{
	packagesSHA: "4bfac0506e85587243502391da7f2bac22f3dc9c71a9e4a2df05bf93fe53d747",
	output:      "/tmp/CompCert/ccomp",
	initialCheck: `test ! -e /tmp/CompCert
test ! -e /tmp/coq-8.16.1
test "$(nproc)" = 2
command -v gcc make git curl
dpkg-query -W gcc make git curl ca-certificates`,
}

const compilerResult = `test "$(git -C /tmp/CompCert rev-parse HEAD)" = 44d67d81b2a9ed5731d2bbfdf56d591ff3046ab5
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
         "archive.ubuntu.com":"http://archive.ubuntu.com/ubuntu/dists/noble/InRelease",
         "security.ubuntu.com":"http://security.ubuntu.com/ubuntu/dists/noble-security/InRelease"}
results = {}
for host, url in sites.items():
    with urllib.request.urlopen(urllib.request.Request(url,method="GET"),timeout=15) as response:
        results[host] = {"addresses":sorted({a[4][0] for a in socket.getaddrinfo(host,80 if url.startswith("http:") else 443)}),"status":response.status,"bytes_read":len(response.read(32))}
print(json.dumps({"route":Path("/proc/net/route").read_text(),"resolv_conf":Path("/etc/resolv.conf").read_text(),"dependency_readback":results}))`
