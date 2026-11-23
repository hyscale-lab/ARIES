"""Build the original corrected relaxed adapter with the source-bound Gloo calibration."""
import argparse
import io
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

HERE=Path(__file__).resolve().parent
REPO=HERE.parents[2]
BASE='229f5d7'
CALIBRATION='35ff527'
def git(*args):
    return subprocess.check_output(['git','-C',REPO,*args])
def main(go,target):
    if target.exists():raise FileExistsError(target)
    with tempfile.TemporaryDirectory() as name:
        tree=Path(name)
        with tarfile.open(fileobj=io.BytesIO(git('archive',BASE))) as packed:packed.extractall(tree,filter='data')
        cmd=tree/'cmd/aries-harbor'
        for f in ('torch_gloo.go','torch_gloo.py'):(cmd/f).write_bytes(git('show',f'{CALIBRATION}:cmd/aries-harbor/{f}'))
        for src,dst in [('follow.go.txt','e047_follow.go'),('wait_state.py','e047_wait_state.py')]:shutil.copyfile(HERE/src,cmd/dst)
        path=cmd/'main.go';data=path.read_text()
        def once(anchor,insert):
            nonlocal data
            assert data.count(anchor)==1,anchor
            data=data.replace(anchor,insert+anchor)
        once('\tid, err := m.Create(ctx, req)', '\tif *e047Alias { req.PrivateHostsFile = true }\n')
        once('\tif *relaxedML {','''\tif *e047Alias && (!*relaxedML || r.Trial != torchGlooTrial || *observer == "" || *sampler == "") {
        return errors.New("e047 hostname calibration requires its reviewed measured original")
    }
''')
        for anchor,action,phase in [('snapshot_start','install','before_snapshot'),('restore_complete','verify','after_restore')]:
            statement=f'\tif err = lifecycle("{anchor}"); err != nil {{\n\t\treturn err\n\t}}'
            instrument=f'\tif *e047Alias {{\n\t\tif err := readTorchGloo(ctx,m,id,"{phase}","{action}",log);err!=nil {{return err}}\n\t}}'
            if phase=='before_snapshot':once(statement,instrument+'\n')
            else:
                assert data.count(statement)==1;data=data.replace(statement,statement+'\n'+instrument)
        once('\tif err := harbor.Run(ctx, r, h); err != nil {','''\tif *e047Alias {
        h.FinishObservedProcess = boundE047Join(m,id,h.FinishObservedProcess,log)
    }
''')
        insert='''\t\tif *e047Alias {
            if err := readTorchGloo(ctx,m,id,"after_source","canary",log);err!=nil {return err}
        }
'''
        anchor='\t\tif hfHostCPU >= 0 {\n\t\t\treturn checkHFCPU'
        once(anchor,insert)
        path.write_text(data)
        subprocess.run([str(go.with_name('gofmt')),'-w',path,cmd/'e047_follow.go'],check=True)
        env=dict(os.environ,CGO_ENABLED='0',GOOS='linux',GOARCH='amd64',GOAMD64='v1')
        subprocess.run([go,'test','-buildvcs=false','./pkg/replay/harbor','./cmd/aries-harbor'],cwd=tree,env=env,check=True)
        subprocess.run([go,'build','-trimpath','-buildvcs=false','-o',target,'./cmd/aries-harbor'],cwd=tree,env=env,check=True)
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('go',type=Path);p.add_argument('output',type=Path);a=p.parse_args();main(a.go.resolve(),a.output.resolve())
