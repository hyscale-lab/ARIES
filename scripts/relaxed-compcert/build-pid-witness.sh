#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
output=$(realpath -m "$1")
test ! -e "$output"
work=$(mktemp -d)
trap 'python3 -c "import shutil,sys;shutil.rmtree(sys.argv[1])" "$work"' EXIT
git -C "$repo" archive de9649feef1fa0ae8e150d26890313cd71ead678 | tar -C "$work" -xf -
cd "$work"
patch --batch --fuzz=0 -p1 < "$here/native.patch"
cp "$here/relaxed_process_test.go.txt" pkg/replay/harbor/relaxed_compcert_test.go
cp "$here/pid-witness.go.txt" cmd/aries-harbor/pid_witness.go
cp "$here/pid-witness.py" cmd/aries-harbor/pid_witness.py
python3 - <<'PY'
from pathlib import Path
p=Path('cmd/aries-harbor/main.go')
s=p.read_text()
old='r.Task != "compile-compcert" || !*processDrift || *opamPID || *opamLog'
new='r.Trial != compilerWaitTrial || !*processDrift || !*opamPID || *opamLog'
assert s.count(old)==1
s=s.replace(old,new)
old='h.Exec = mapCompilerWait(h.Exec, log.Encode)'
new='''h.Exec = witnessCompilerWait(mapCompilerWait(h.Exec, log.Encode), func(ctx context.Context, phase, pid string)(core.CommandResult,error){
            return m.Exec(ctx,id,core.Command{Path:"/usr/bin/python3",Args:[]string{"-c",compilerPIDWitness,phase,pid}})
        },log.Encode)'''
assert s.count(old)==1;p.write_text(s.replace(old,new))
PY
gofmt -w cmd/aries-harbor/pid_witness.go cmd/aries-harbor/main.go
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1
go test -buildvcs=false ./pkg/replay/harbor ./cmd/aries-harbor
go build -trimpath -buildvcs=false -o "$output" ./cmd/aries-harbor
sha256sum "$output"
