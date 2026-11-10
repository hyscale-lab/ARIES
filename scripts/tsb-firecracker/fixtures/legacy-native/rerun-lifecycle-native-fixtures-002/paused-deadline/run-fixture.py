#!/usr/bin/env python3
from pathlib import Path
import subprocess,json,os,time,urllib.request,signal,hashlib
REPO=Path('/home/nehalem/aries')
ROOT=Path(__file__).resolve().parent
os.umask(0o077)
mock=observer=aries=mcp=None
def save(name,value):(ROOT/name).write_text(json.dumps(value,indent=2)+'\n')
def ready(url,process):
 deadline=time.monotonic()+45
 while time.monotonic()<deadline:
  if process.poll() is not None:raise RuntimeError('server exited before ready')
  try:
   with urllib.request.urlopen(url,timeout=1) as response:
    value=json.load(response)
   if value.get('ready'):return value
  except Exception:time.sleep(.1)
 raise TimeoutError('server readiness timeout')
try:
 with (ROOT/'setup.log').open('x') as log:
  setup=subprocess.run([str(Path('/home/nehalem/aries/runs/tsb-access-dirty-001/rerun-lifecycle-implementation-001/bin/aries')),'setup',str(ROOT/'profile.json')],cwd=REPO,stdout=log,stderr=subprocess.STDOUT,env={**os.environ,'ECHO_API_KEY':'aries-native-overlap-fixture'})
 save('setup-exit.json',{'exit_code':setup.returncode})
 if setup.returncode:raise RuntimeError('ARIES setup failed')
 mcp=subprocess.Popen(['python3',str(ROOT/'mock-mcp.py')],cwd=REPO,stdout=(ROOT/'mcp-server.log').open('x'),stderr=subprocess.STDOUT)
 ready('http://127.0.0.1:18083/health',mcp)
 mock=subprocess.Popen(['python3',str(ROOT/'mock-model.py')],cwd=REPO,env={**os.environ,'ARIES_FIXTURE_HANG_SECOND':'1'},stdout=(ROOT/'mock-server.log').open('x'),stderr=subprocess.STDOUT)
 ready('http://127.0.0.1:18082/health',mock)
 observer=subprocess.Popen(['python3',str(REPO/'scripts/tsb-pages/serve.py'),'--mode','group','--output',str(ROOT/'observer'),'--vm-prefix','aries-sandbox-fix-git-001-','--port','18182','--access-dirty-config',str(ROOT/'access-dirty-config.json'),'--attribution-config',str(ROOT/'attribution-config.json')],cwd=REPO,stdout=(ROOT/'observer-server.log').open('x'),stderr=subprocess.STDOUT)
 save('observer-ready.json',ready('http://127.0.0.1:18182/',observer))
 before=time.monotonic_ns()
 aries=subprocess.Popen([str(Path('/home/nehalem/aries/runs/tsb-access-dirty-001/rerun-lifecycle-implementation-001/bin/aries')),str(ROOT/'profile.json')],cwd=REPO,stdout=(ROOT/'aries.log').open('x'),stderr=subprocess.STDOUT,env={**os.environ,'ECHO_API_KEY':'aries-native-overlap-fixture'})
 try:code=aries.wait(timeout=360)
 except subprocess.TimeoutExpired:
  aries.send_signal(signal.SIGTERM)
  code=aries.wait(timeout=180)
  raise RuntimeError('ARIES fixture exceeded bounded time')
 save('aries-exit.json',{'exit_code':code,'begin_monotonic_ns':before,'end_monotonic_ns':time.monotonic_ns(),'paid_requests':0})
 save('expected-deadline.json',{'expected_agent_cancellation':True,'actual_exit_code':code,'requires_independent_run_result_and_resume_before_evaluation_validation':True})
finally:
 if aries and aries.poll() is None:
  aries.send_signal(signal.SIGTERM)
  aries.wait(timeout=180)
 for process in [observer,mock,mcp]:
  if process and process.poll() is None:
   process.send_signal(signal.SIGTERM);process.wait(timeout=30)
