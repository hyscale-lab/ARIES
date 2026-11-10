#!/usr/bin/env python3
from pathlib import Path
from http.server import BaseHTTPRequestHandler,HTTPServer
import json,os,signal,threading,time
ROOT=Path(__file__).resolve().parent
os.umask(0o077)
log=(ROOT/'mcp-requests.jsonl').open('x')
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_GET(self):
  if self.path=='/health':
   data=json.dumps({'ready':True,'scope':'deterministic fixture MCP; no advertised parallel support'}).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
  else:self.send_response(405);self.send_header('Allow','POST');self.send_header('Content-Length','0');self.end_headers()
 def do_DELETE(self):self.send_response(200);self.send_header('Content-Length','0');self.end_headers()
 def do_POST(self):
  body=json.loads(self.rfile.read(int(self.headers.get('Content-Length','0'))))
  method=body.get('method');row={'method':method,'wall_time_ns':time.time_ns(),'monotonic_ns':time.monotonic_ns()}
  if 'id' not in body:
   log.write(json.dumps(row)+'\n');log.flush();self.send_response(202);self.send_header('Content-Length','0');self.end_headers();return
  error=None;result={}
  if method=='initialize':
   result={'protocolVersion':body.get('params',{}).get('protocolVersion','2025-03-26'),'capabilities':{'tools':{'listChanged':False}},'serverInfo':{'name':'aries-sequential-noop-fixture','version':'1.0.0'}}
  elif method=='tools/list':
   result={'tools':[{'name':'fixture_serial_noop','description':'Deterministic fixture-only noop; native MCP default sequential execution.','inputSchema':{'type':'object','properties':{},'additionalProperties':False}}]}
  elif method=='tools/call':
   steps=[json.loads(l) for l in (ROOT/'observer/steps.jsonl').read_text().splitlines() if l.strip()]
   first_ends=[s for s in steps if s.get('event')=='step_end' and s.get('tool_call_id')=='serialexec']
   row['first_exec_end_already_recorded']=len(first_ends)==1
   row['first_exec_end_ack']=first_ends[0].get('observer_ack') if first_ends else None
   groupfile=ROOT/'observer/groups.jsonl'
   groups=[json.loads(l) for l in groupfile.read_text().splitlines() if l.strip()] if groupfile.exists() else []
   row['completed_group_ids_before_noop']=[g.get('group_id') for g in groups if g.get('event')=='group_end']
   row['group_collected_before_sequential_noop']=any(g.get('event')=='group_end' and 'serialexec' in g.get('expected_tool_call_ids',g.get('member_tool_call_ids',[])) for g in groups)
   time.sleep(.3)
   result={'content':[{'type':'text','text':'SERIAL_NOOP_DONE'}],'isError':False}
  elif method=='ping':result={}
  else:error={'code':-32601,'message':'Unsupported fixture MCP method'}
  row['tool_name']=body.get('params',{}).get('name');log.write(json.dumps(row)+'\n');log.flush()
  data=json.dumps({'jsonrpc':'2.0','id':body['id'],**({'error':error} if error else {'result':result})}).encode()
  self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
server=HTTPServer(('0.0.0.0',18083),Handler)
def stop(*_):threading.Thread(target=server.shutdown,daemon=True).start()
signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
(ROOT/'mcp-ready.json').write_text(json.dumps({'pid':os.getpid(),'port':18083,'supportsParallelToolCalls':'not advertised; native default sequential'})+'\n')
try:server.serve_forever(poll_interval=.2)
finally:server.server_close();log.close()
