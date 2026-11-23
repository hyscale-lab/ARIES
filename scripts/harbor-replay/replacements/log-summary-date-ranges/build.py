"""Build the adapter that can execute only the five reviewed local log sessions."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import os

HERE=Path(__file__).resolve().parent
ARIES=HERE.parents[3]
BASE='de9649feef1fa0ae8e150d26890313cd71ead678'
RELAXED='b091b7c'
def replace(p,old,new):
    text=p.read_text()
    if text.count(old)!=1:raise ValueError(f'overlay context changed in {p}')
    p.write_text(text.replace(old,new))
def main(output):
    if output.exists():raise FileExistsError(output)
    with tempfile.TemporaryDirectory() as temp:
        work=Path(temp)
        archive=subprocess.Popen(['git','-C',ARIES,'archive',BASE],stdout=subprocess.PIPE)
        with tarfile.open(fileobj=archive.stdout,mode='r|') as t:t.extractall(work,filter='data')
        if archive.wait():raise RuntimeError('source archive failed')
        patch=subprocess.check_output(['git','-C',ARIES,'show',RELAXED+':scripts/relaxed-compcert/native.patch'])
        subprocess.run(['patch','--batch','--fuzz=0','-p1'],input=patch,cwd=work,check=True)
        cmd=work/'cmd/aries-harbor'
        (work/'pkg/replay/harbor/reviewed.json').write_bytes((HERE/'reviewed.json').read_bytes())
        p=cmd/'main.go'
        replace(p,'if r.Task != "compile-compcert" || !*processDrift','if r.Task != "log-summary-date-ranges" || !*processDrift')
        replace(p,'this measurement binary requires a reviewed CompCert original','this measurement binary requires a reviewed log-summary-date-ranges original')
        replace(p,'allowed := grpcPoll ||','allowed := r.Task == "log-summary-date-ranges" || grpcPoll ||')
        p=cmd/'prepare.go'
        replace(p,'if r.Trial == debianPatchTrial ||','if r.Trial == "56d7a906-0fc0-4101-8987-6476d32648c1" || r.Trial == debianPatchTrial ||')
        start='cmd := "set -eu\\ntest -s " + task.output + "\\nsha256sum " + task.output + "\\n" + task.inputCheck'
        replace(p,start,start+'\n if r.Task == "log-summary-date-ranges" { cmd += logRangeVerify }')
        task=json.loads((HERE/'provenance.json').read_bytes())
        verifier=(HERE/'verifier.py.txt').read_bytes()
        assert hashlib.sha256(verifier).hexdigest()==task['task_files']['tests/test_outputs.py']
        check=("""
python3 - <<'ARIESFINAL'
"""+verifier.decode()+"""
test_summary_file_exists()
test_summary_structure_and_counts()
print("OFFICIAL_LOG_SUMMARY_TESTS 2/2")
print(OUTPUT_FILE.read_text(), end="")
ARIESFINAL
expected=MANIFEST
actual=$(LC_ALL=C sha256sum /app/logs/*.log | sha256sum | cut -d ' ' -f 1)
test "$actual" = "$expected"
printf 'INPUT_LOG_MANIFEST %s\\n' "$actual"
""").replace('MANIFEST\n',''+task['registry_input']['manifest_sha256']+'\n',1)
        ready="""test ! -e /app/summary.csv && test -d /app/logs && test ! -e /app/log_generator_deterministic.py
rg_binary=$(command -v rg)
test -x "$rg_binary"
if ! test -e /usr/local/bin/rg; then ln -s "$rg_binary" /usr/local/bin/rg; fi
sha256sum /usr/local/bin/rg
printf '%s\\n' 'export PATH=/root/.nvm/versions/node/v22.23.2/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin' > /etc/profile.d/zz-aries-log-source.sh
"""
        task_go='package main\n\nconst logRangeVerify = '+json.dumps(check)+'\n\nfunc init() { taskSetups["log-summary-date-ranges"] = taskSetup{output:"/app/summary.csv",initialCheck:'+json.dumps(ready)+'} }\n'

        (cmd/'log_summary.go').write_text(task_go)
        env=dict(os.environ,CGO_ENABLED='0',GOOS='linux',GOARCH='amd64',GOAMD64='v1')
        # Other workload registry allowlist tests belong to the baseline binary.
        subprocess.run(['go','test','-buildvcs=false','./cmd/aries-harbor','./pkg/replay/harbor','-run','Test.*(Process|Yield|Parallel)'],cwd=work,env=env,check=True)
        subprocess.run(['go','build','-buildvcs=false','-trimpath','-o',output.resolve(),'./cmd/aries-harbor'],cwd=work,env=env,check=True)
    print(hashlib.sha256(output.read_bytes()).hexdigest(),output)
if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('output',type=Path);main(p.parse_args().output)
