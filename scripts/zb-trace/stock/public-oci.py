"""Fetch the exact public Harbor manifests and blobs used by a reviewed TB fixture."""
import argparse
import hashlib
import json
import os
from pathlib import Path
from urllib.request import Request,urlopen
from urllib.parse import urlencode

repo='harborframework/terminal-bench'
base='https://registry-1.docker.io/v2/'+repo
media=', '.join(['application/vnd.oci.image.manifest.v1+json','application/vnd.docker.distribution.manifest.v2+json'])
def fetch(target,digests):
    target.mkdir(parents=True,exist_ok=True)
    auth='https://auth.docker.io/token?'+urlencode({'service':'registry.docker.io','scope':'repository:'+repo+':pull'})
    with urlopen(auth,timeout=30) as f:headers={'Authorization':'Bearer '+json.load(f)['token'],'Accept':media}
    def get(digest,kind):
        import re
        if not re.fullmatch(r'sha256:[0-9a-f]{64}',digest):raise ValueError('invalid image digest')
        sha=digest[7:]; path=target/sha
        if path.exists():
            with path.open('rb') as f:
                if hashlib.file_digest(f,'sha256').hexdigest()==sha:return path
            raise ValueError('existing file differs from addressed digest')
        tmp=path.with_suffix('.'+str(os.getpid())+'.part')
        # Registry redirects to signed blob storage. Do not forward registry credentials across origins.
        import urllib.request
        class RegistryRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self,req,fp,code,msg,hdrs,newurl):
                new=super().redirect_request(req,fp,code,msg,hdrs,newurl)
                if new is not None and new.host!=req.host:new.remove_header('Authorization')
                return new
        with urllib.request.build_opener(RegistryRedirect).open(Request(base+'/'+kind+'/'+digest,headers=headers),timeout=150) as f,tmp.open('wb') as out:
            import shutil
            shutil.copyfileobj(f,out)
        with tmp.open('rb') as f:
            if hashlib.file_digest(f,'sha256').hexdigest()!=sha:raise ValueError('download differs from descriptor')
        tmp.replace(path);return path
    for d in digests:
        m=json.loads(get(d,'manifests').read_bytes())
        for blob in [m['config'],*m['layers']]:
            assert get(blob['digest'],'blobs').stat().st_size==blob['size']
        print(d,len(m['layers']),sum(b['size'] for b in m['layers']),flush=True)
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('target',type=Path);p.add_argument('digests',nargs='+')
    a=p.parse_args();fetch(a.target,a.digests)
