#!/usr/bin/env python3
"""Worker-only deterministic page-window mechanism experiment using native vHive execution."""
import argparse,json,os,selectors,signal,subprocess,sys,threading,time
from pathlib import Path
from idle import IdleObserver
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'tsb-access'))
from capture_slots import parse_slot

class Driver:
    def __init__(self,args,log):
        self.log=log
        self.proc=subprocess.Popen(args,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=log,bufsize=0)
        self.buf=bytearray();self.sel=selectors.DefaultSelector();self.sel.register(self.proc.stdout,selectors.EVENT_READ)
    def receive(self,timeout=180):
        deadline=time.monotonic()+timeout
        while b'\n' not in self.buf:
            if not self.sel.select(max(0,deadline-time.monotonic())):raise TimeoutError('driver response timeout')
            b=os.read(self.proc.stdout.fileno(),65536)
            if not b:raise RuntimeError('driver ended; see driver stderr')
            self.buf.extend(b)
        line,_,remain=self.buf.partition(b'\n');self.buf[:]=remain
        return json.loads(line)
    def call(self,request):
        self.proc.stdin.write((json.dumps(request)+'\n').encode());self.proc.stdin.flush()
        result=self.receive(900)
        if result.get('id')!=request['id']:raise RuntimeError('response ID mismatch')
        return result
    def close(self):
        if self.proc.poll() is None:
            self.proc.stdin.close()
            try:return self.proc.wait(timeout=150)
            except subprocess.TimeoutExpired:
                self.proc.send_signal(signal.SIGTERM)
                return self.proc.wait(timeout=150)
        return self.proc.returncode

def main():
    ap=argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--output',type=Path,required=True)
    ap.add_argument('--commands',type=Path,required=True,help='JSON argv lists for no-model mechanism path')
    a=ap.parse_args()
    os.umask(0o077);a.output.mkdir(parents=True,exist_ok=False)
    manifest={'mode':'persistent-idle','resources':{'vcpu':2,'memory_mib':4096},'model':None,'paid_requests':0,'boundary':'arm before native tool dispatch; collect after full command completion; no periodic samples','scope':'deterministic native-exec mechanism pilot; not LLM steps or benchmark score','started_utc':time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime())}
    (a.output/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
    events=[];tracer=None;driver=None;observer=None
    out=(a.output/'windows.jsonl').open('x');requests=(a.output/'model.jsonl').open('x')
    def save(f,row):f.write(json.dumps(row)+'\n');f.flush()
    try:
        daemon=int(subprocess.check_output(['systemctl','show','firecracker-containerd','--property','MainPID','--value'],text=True).strip())
        tracer=subprocess.Popen(['strace','-f','-e','trace=ioctl','-p',str(daemon)],stderr=subprocess.PIPE,stdout=subprocess.DEVNULL,text=True)
        def trace():
            for line in tracer.stderr:
                e=parse_slot(line)
                if e:events.append(e)
        thread=threading.Thread(target=trace);thread.start();time.sleep(1)
        if tracer.poll() is not None:raise RuntimeError('slot capture attach failed')
        driver=Driver(['bin/tsb-pages-vm','-output',str(a.output/'vm')],(a.output/'driver-stderr.log').open('x'))
        ready=driver.receive();(a.output/'ready.json').write_text(json.dumps(ready,indent=2)+'\n')
        if ready.get('event')!='ready':raise RuntimeError('driver not ready')
        tracer.send_signal(signal.SIGINT);tracer.wait(timeout=10);thread.join(timeout=10);tracer=None
        (a.output/'slots.json').write_text(json.dumps(events,indent=2)+'\n')
        pids={int(next(l.split()[1] for l in Path('/proc/'+str(e['tid'])+'/status').read_text().splitlines() if l.startswith('Tgid:'))) for e in events}
        if len(pids)!=1:raise RuntimeError('slot capture requires exactly one VMM')
        observer=IdleObserver(pids.pop(),ready['vm_id'],events)
        def execute(call_id,argv):
            req={'id':call_id,'argv':argv,'cwd':'/','timeout_seconds':180}
            observer.arm(call_id);result=driver.call(req);reading=observer.collect()
            save(out,{'tool_call_id':call_id,'request':req,'result':result,'reading':reading})
            print(call_id,'exit',result.get('exit_code'),'pages',len(reading['accessed_gpas']),flush=True)
            return result
        for i,argv in enumerate(json.loads(a.commands.read_text())):execute('deterministic-'+str(i+1),argv)
    except BaseException as e:
        (a.output/'failure.json').write_text(json.dumps({'type':type(e).__name__,'message':str(e)})+'\n');raise
    finally:
        if observer:observer.close()
        if tracer and tracer.poll() is None:tracer.send_signal(signal.SIGINT);tracer.wait(timeout=10)
        if driver:
            code=driver.close();(a.output/'driver-exit.json').write_text(json.dumps({'exit_code':code})+'\n')
        out.close();requests.close()
if __name__=='__main__':main()
