import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {spawn} from "node:child_process";
import plugin from "./index.mjs";

test("native declarations bracket sequential, immediate and parallel batches without changing messages",async()=>{
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),"aries-llm-step-plugin-"));
 const serverCode=[
 "import http from 'node:http';import fs from 'node:fs';",
 "let pending;",
 "const server=http.createServer(async(req,res)=>{",
 " let body='';for await(const c of req)body+=c;const value=JSON.parse(body);",
 " fs.appendFileSync(process.env.CALLS,JSON.stringify({url:req.url,body:value})+'\\n');",
 " let collection='pending';",
 " if(req.url==='/batch'){pending=new Set(value.expected_tool_call_ids);collection='armed';}",
 " if(req.url==='/end'){pending.delete(value.tool_call_id);collection=pending.size===0?'collected':'pending';}",
 " setTimeout(()=>{res.writeHead(200,{'content-type':'application/json'});res.end(JSON.stringify({ok:true,collection,batch_id:value.batch_id,llm_step_id:value.llm_step_id,remaining_expected_tool_call_ids:[...pending],granularity:'llm_reasoning_step'}))},req.url==='/batch'?100:40);",
 "});",
 "server.listen(0,'127.0.0.1',()=>console.log(server.address().port));"
 ].join("\n");
 const server=spawn(process.execPath,["--input-type=module","-e",serverCode],{env:{...process.env,CALLS:path.join(dir,"calls.jsonl")},stdio:["ignore","pipe","inherit"]});
 try{
  const port=await new Promise((resolve,reject)=>{server.stdout.once("data",b=>resolve(Number(b.toString().trim())));server.once("error",reject)});
  Object.assign(process.env,{ARIES_STEP_OBSERVER_URL:"http://127.0.0.1:"+port,ARIES_STEP_SESSION_ID:"task-1",ARIES_STEP_TURN_ID:"user-1",ARIES_STEP_LOG_PATH:path.join(dir,"plugin.jsonl")});
  const hooks={};let service;
  plugin.register({on:(name,handler)=>hooks[name]=handler,registerService:s=>service=s});
  service.start({stateDir:dir});
  const scoped={};plugin.register({on:(name,handler)=>scoped[name]=handler,registerService:()=>{}});Object.assign(hooks,scoped);
  const ctx={sessionKey:"native-key",sessionId:"native-session",runId:"run-1"};
  function declare(ids,stamp){
   const message={role:"assistant",stopReason:"toolUse",timestamp:stamp,provider:"fixture",model:"fixture",id:"assistant-"+stamp,
    content:[{type:"text",text:"DO-NOT-LOG"},...ids.map(id=>({type:"toolCall",id,name:"exec",arguments:{secret:"DO-NOT-LOG"}}))]};
   const original=JSON.stringify(message);const start=performance.now();
   assert.equal(hooks.before_message_write({message},ctx),undefined);
   assert.ok(performance.now()-start>=90,"assistant persistence passed before batch arm acknowledgement");
   assert.equal(JSON.stringify(message),original);
  }
  async function begin(id){assert.equal(await hooks.before_tool_call({toolCallId:id,toolName:"exec",params:{secret:"DO-NOT-LOG"}},{...ctx,toolCallId:id}),undefined)}
  function end(id,error=false){
   const message={role:"toolResult",content:[{type:"text",text:"DO-NOT-LOG"}],isError:error};const original=JSON.stringify(message);const start=performance.now();
   assert.equal(hooks.tool_result_persist({toolCallId:id,message},{...ctx,toolCallId:id}),undefined);
   assert.ok(performance.now()-start>=35,"result persistence passed before observer acknowledgement");
   assert.equal(JSON.stringify(message),original);
  }
  declare(["a","b"],1000);await begin("a");end("a");await begin("b");end("b");
  declare(["a","b"],2000);end("a",true);end("b",true);
  declare(["q","r"],3000);await Promise.all([begin("q"),begin("r")]);end("q");end("r");
  hooks.tool_result_persist({toolCallId:"synthetic",isSynthetic:true,message:{}},ctx);
  hooks.tool_result_persist({toolCallId:"missing",message:{}},ctx);
  service.stop();
  const requests=fs.readFileSync(path.join(dir,"calls.jsonl"),"utf8").trim().split("\n").map(JSON.parse);
  assert.deepEqual(requests.map(r=>r.url),["/batch","/begin","/end","/begin","/end","/batch","/end","/end","/batch","/begin","/begin","/end","/end"]);
  const declarations=requests.filter(r=>r.url==="/batch");
  assert.deepEqual(declarations.map(r=>r.body.expected_tool_call_ids),[["a","b"],["a","b"],["q","r"]]);
  assert.equal(new Set(declarations.map(r=>r.body.batch_id)).size,3);
  for(const request of requests){assert.equal(request.body.session_id,"task-1");assert.equal(request.body.turn_id,"user-1");assert.equal(request.body.batch_id,request.body.llm_step_id)}
  const text=fs.readFileSync(path.join(dir,"plugin.jsonl"),"utf8");assert.ok(!text.includes("DO-NOT-LOG"));assert.ok(!JSON.stringify(requests).includes("DO-NOT-LOG"));
  const rows=text.trim().split("\n").map(JSON.parse);
  assert.equal(rows.filter(r=>r.event==="batch_confirmed").length,3);
  assert.equal(rows.filter(r=>r.event==="batch_completed").length,3);
  assert.equal(rows.filter(r=>r.event==="end_confirmed"&&r.observer_ack.collection==="pending").length,3);
  assert.equal(rows.filter(r=>r.event==="end_confirmed"&&r.observer_ack.collection==="collected").length,3);
  assert.deepEqual(rows.filter(r=>r.event==="measurement_error").map(r=>r.reason),["synthetic_result","result_without_declared_batch"]);
  assert.equal(rows.at(-1).unfinished_batches,0);assert.equal(rows.at(-1).unfinished_steps,0);
  assert.equal(new Set(rows.map(r=>r.plugin_pid)).size,1);
  assert.notEqual(rows.find(r=>r.event==="service_started").registration,rows.find(r=>r.event==="batch_requested").registration);
 }finally{server.kill();await new Promise(resolve=>server.once("exit",resolve));fs.rmSync(dir,{recursive:true,force:true})}
});
