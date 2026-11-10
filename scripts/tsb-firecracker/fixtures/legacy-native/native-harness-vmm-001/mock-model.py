#!/usr/bin/env python3
from pathlib import Path
from http.server import BaseHTTPRequestHandler,HTTPServer
import json,os,signal,threading,time,hashlib
ROOT=Path(__file__).resolve().parent
os.umask(0o077)
log=(ROOT/'mock-requests.jsonl').open('x')
request_number=0
def text(content):
 if isinstance(content,str):return content
 if isinstance(content,list):return '\n'.join(str(x.get('text','')) for x in content if isinstance(x,dict))
 return str(content)
def parallel_command(own,other,label):
 return ('set -eu; printf started > /tmp/aries-llm-batch-parallel-'+own+'; i=0; '
  'until test -f /tmp/aries-llm-batch-parallel-'+other+'; do i=$((i+1)); if test "$i" -gt 160; then echo PARALLEL_'+label+'_TIMEOUT; exit 23; fi; sleep 0.05; done; sleep 1; echo PARALLEL_'+label+'_DONE; pwd')
def call_payload(ids,names,args):
 return {'role':'assistant','tool_calls':[{'index':i,'id':cid,'type':'function','function':{'name':names[i],'arguments':json.dumps(args[i])}} for i,cid in enumerate(ids)]}
def completed_groups():
 p=ROOT/'observer/groups.jsonl'
 return [g for g in (json.loads(l) for l in p.read_text().splitlines() if l.strip()) if g.get('event')=='group_end'] if p.exists() else []
def require_collected(row,count):
 groups=completed_groups()
 row['completed_group_ids_at_request']=[g['group_id'] for g in groups]
 row['group_collection_precedes_request']=len(groups)==count and all(g['collection_complete_wall_time_ns']<row['wall_time_ns'] for g in groups)
 if not row['group_collection_precedes_request']:raise ValueError('next model request preceded expected complete LLM batch collection')
class Handler(BaseHTTPRequestHandler):
 def log_message(self,*args):pass
 def do_GET(self):
  value={'object':'list','data':[{'id':'aries-echo','object':'model','owned_by':'aries-native-llm-batch-fixture'}]} if self.path.rstrip('/')=='/v1/models' else {'ready':True,'scope':'deterministic mock; no external model'}
  data=json.dumps(value).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
 def do_POST(self):
  global request_number
  request_number+=1
  raw=self.rfile.read(int(self.headers.get('Content-Length','0')));body=json.loads(raw)
  results={m.get('tool_call_id'):text(m.get('content')) for m in body.get('messages',[]) if m.get('role')=='tool'}
  row={'request_index':request_number,'wall_time_ns':time.time_ns(),'monotonic_ns':time.monotonic_ns(),'path':self.path,'model':body.get('model'),'stream':body.get('stream'),'request_sha256':hashlib.sha256(raw).hexdigest(),'tool_result_ids':sorted(results),'roles':[m.get('role') for m in body.get('messages',[])],'parallel_tool_calls_param':body.get('parallel_tool_calls')}
  try:
   if self.path!='/v1/chat/completions' or body.get('model')!='aries-echo' or body.get('stream') is not True:raise ValueError('unexpected route/model/stream')
   names=[t.get('function',{}).get('name','') for t in body.get('tools',[])]
   noop=next((n for n in names if n.endswith('fixture_serial_noop')),None)
   if not noop or 'exec' not in names:raise ValueError('required native exec/MCP tools missing: '+','.join(names))
   if not results:
    if request_number!=1:raise ValueError('unexpected repeated initial model request')
    ids=['parallela','parallelb'];tool_names=['exec','exec'];args=[{'command':parallel_command('a','b','A'),'yieldMs':30000,'timeout':30},{'command':parallel_command('b','a','B'),'yieldMs':30000,'timeout':30}]
    delta=call_payload(ids,tool_names,args);finish='tool_calls';row['issued_tool_call_ids']=ids;row['issued_tool_names']=tool_names;row['native_policy']='default parallel exec'
   elif set(results)=={'parallela','parallelb'}:
    if request_number!=2:raise ValueError('unexpected repeated second model request')
    if not all('PARALLEL_'+label+'_DONE' in results[cid] and 'TIMEOUT' not in results[cid] for cid,label in [('parallela','A'),('parallelb','B')]):raise ValueError('parallel mutual marker test failed')
    row['both_mutual_marker_commands_passed']=True;require_collected(row,1)
    ids=['serialexec','serialnoop'];tool_names=['exec',noop];args=[{'command':'sleep 1; echo SERIAL_EXEC_DONE; pwd','yieldMs':30000,'timeout':30},{}]
    delta=call_payload(ids,tool_names,args);finish='tool_calls';row['issued_tool_call_ids']=ids;row['issued_tool_names']=tool_names;row['native_policy']='MCP default executionMode sequential selects native sequential batch scheduler'
   elif set(results)=={'parallela','parallelb','serialexec','serialnoop'}:
    if request_number!=3:raise ValueError('unexpected repeated final model request')
    if 'SERIAL_EXEC_DONE' not in results['serialexec'] or 'SERIAL_NOOP_DONE' not in results['serialnoop']:raise ValueError('sequential result completion marker missing')
    mcp=[json.loads(l) for l in (ROOT/'mcp-requests.jsonl').read_text().splitlines() if l.strip()]
    executed=[e for e in mcp if e.get('method')=='tools/call']
    row['native_sequential_execution_proven']=len(executed)==1 and executed[0].get('first_exec_end_already_recorded') is True
    row['no_group_collect_between_native_serial_calls']=len(executed)==1 and executed[0].get('group_collected_before_sequential_noop') is False
    if not row['native_sequential_execution_proven'] or not row['no_group_collect_between_native_serial_calls']:raise ValueError('sequential batch ordering/outer envelope failed')
    row['both_sequential_command_markers_passed']=True;require_collected(row,2)
    delta={'role':'assistant','content':'Native LLM-batch fixture completed: parallel and sequential batches returned all four tool results.'};finish='stop'
   else:raise ValueError('unexpected tool-result set')
   row['response_finish_reason']=finish;log.write(json.dumps(row)+'\n');log.flush()
   self.send_response(200);self.send_header('Content-Type','text/event-stream');self.send_header('Cache-Control','no-cache');self.send_header('Connection','close');self.end_headers()
   common={'id':'aries-native-batch-'+str(request_number),'object':'chat.completion.chunk','created':int(time.time()),'model':'aries-echo'}
   for payload in [{**common,'choices':[{'index':0,'delta':delta,'finish_reason':None}]},{**common,'choices':[{'index':0,'delta':{},'finish_reason':finish}],'usage':{'prompt_tokens':1,'completion_tokens':1,'total_tokens':2}}]:self.wfile.write(('data: '+json.dumps(payload)+'\n\n').encode())
   self.wfile.write(b'data: [DONE]\n\n');self.wfile.flush()
  except Exception as error:
   row['error']=str(error);log.write(json.dumps(row)+'\n');log.flush();data=json.dumps({'error':{'message':str(error)}}).encode();self.send_response(400);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
server=HTTPServer(('0.0.0.0',18082),Handler)
def stop(*_):threading.Thread(target=server.shutdown,daemon=True).start()
signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
(ROOT/'mock-ready.json').write_text(json.dumps({'pid':os.getpid(),'port':18082,'paid_requests':0})+'\n')
try:server.serve_forever(poll_interval=.2)
finally:server.server_close();log.close()
