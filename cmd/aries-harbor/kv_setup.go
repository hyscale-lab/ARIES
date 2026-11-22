package main

import "fmt"

const kvTask = "kv-store-grpc"
const kvPythonSHA = "7f8a93c102db3ce25dae68cc485877f8ec9f79edb42a7c4518c96a1153b78682"

// The published task image has Python 3.13.7. The source trial inspected
// Python 3.13.15 and pip 26.2.1 before installing the four application wheels.
var kvSetup = taskSetup{
	output: "server.py",
	initialCheck: `test ! -e /app/server.py && test ! -e /app/kv-store.proto
test "$(python3 --version)" = "Python 3.13.15"
python3 -c 'import importlib.util, pip; assert pip.__version__ == "26.2.1" and importlib.util.find_spec("grpc") is None'
cat > /etc/aries-kv-constraints.txt <<'CONSTRAINTS'
grpcio==1.73.0
grpcio-tools==1.73.0
protobuf==6.33.6
setuptools==84.0.0
CONSTRAINTS
test ! -e /etc/pip.conf
printf '[install]\nconstraint = /etc/aries-kv-constraints.txt\nreport = /tmp/aries-kv-install.json\n' > /etc/pip.conf`,
}

var kvArtifactDigests = map[string][2]string{
	"33193a43-deac-4006-95ee-1c67ea14e3a7": {"6087fc59fbc8bbedc0036fab7fee07c1b3a3b7e2639ed47ec31d24e465563eaa", "d0f3a0821e28da28660c683ab02a02502dd7822d60b6abc1cfca1f74814dbdc1"},
	"3d65a858-41e6-4886-9882-005b662be181": {"0528d1999180678c58e2c11e0d560671394252303e06ffffe0b2f13da4409b05", "e513b9dc79100d04314caa6635cc34ef8f0b9a4ce826e9e90ab0fce511de3cda"},
	"69ba9b75-cf5b-4722-94f4-7ce3ea31bbb4": {"822722b9b7e5e1347e18ee18cb9c1b433f35660660d186eea911986fa14067af", "c5d89a5af749a7cfe46eb7326dfc44849911ee40fba8b947fc9b101423abf8f1"},
	"720d990f-65c3-4e3a-8613-c813937dd7d4": {"e5ceaad1a5f16df21fab4d344e89ceb11dc2b10fbd50431f9ee98a2fed19001d", "e868a72bb2e4ca2a9f1ea1acbb6fc06caa48fccee177bbc613026cd6e3af8f7a"},
	"7e4b1493-d5d7-4b67-a013-07fd0fb6172c": {"0f6cf51d7b35811773f78008475e09d719be6be54c501df94b74ef5c61e8116c", "1db282117b6cea713b6f24b89f4ae14388691ad4284607f3b109469c68c0326b"},
}

func kvFinalCheck(trial string) string {
	hashes := kvArtifactDigests[trial]
	return fmt.Sprintf("\necho '%s  kv-store.proto' | sha256sum -c -\necho '%s  server.py' | sha256sum -c -\n", hashes[0], hashes[1]) + kvServiceCheck
}

const kvServiceCheck = `
python3 - <<'PYREPORT'
import json
from pathlib import Path
import grpc, kv_store_pb2, kv_store_pb2_grpc
expected = {
  "grpcio": "a73c72922dfd30b396a5f25bb3a4590195ee45ecde7ee068acb0892d2900cf07",
  "grpcio-tools": "0bdf66c37431fec41b40f209d1caa6e38f743cf414e727d9b61c9d2d83b04e52",
  "protobuf": "e9db7e292e0ab79dd108d7f1a94fe31601ce1ee3f7b79e0692043423020b0593",
  "setuptools": "51a52592b3b99e102b609654876bd65f19f999935166d1352678931132b0c670"
}
wheels = json.loads(Path("/tmp/aries-kv-install.json").read_text())["install"]
found = {w["metadata"]["name"].lower().replace("_","-"):w["download_info"]["archive_info"]["hashes"]["sha256"] for w in wheels}
assert found == expected, found
with grpc.insecure_channel("127.0.0.1:5328") as channel:
    stub = kv_store_pb2_grpc.KVStoreStub(channel)
    assert stub.GetVal(kv_store_pb2.GetValRequest(key="startup-healthcheck"), timeout=5).val == 0
print("Four recorded wheel digests verified; restored guest service answered GetVal")
PYREPORT`
