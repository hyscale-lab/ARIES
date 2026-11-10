import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {spawn} from "node:child_process";
import plugin from "./index.mjs";

test("compaction end preserves paused session and repeated native model identity has distinct occurrences",async()=>{
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),"aries-compaction-"));
 const child=spawn(process.execPath,["--input-type=module","-e",String.raw`
 import http from "node:http";import fs from "node:fs";
 let pending=[];
 const s=http.createServer(async(req,res)=>{let b="";for await(const c of req)b+=c;
 const v=JSON.parse(b);fs.appendFileSync(process.env.CALLS,JSON.stringify({path:req.url,body:v})+"\n");
 if(req.url==="/batch")pending=v.expected_tool_call_ids;
 if(req.url==="/end")pending=pending.filter(id=>id!==v.tool_call_id);
 const lifecycle=req.url==="/session/start"?{paused:true}:req.url==="/session/end"?{released:true,paused:false,terminal_complete:v.terminal_complete}:{};
 res.writeHead(200,{"content-type":"application/json"});res.end(JSON.stringify({ok:true,session_id:v.session_id,turn_id:v.turn_id,
 ...lifecycle,batch_id:v.batch_id,llm_step_id:v.llm_step_id,remaining_expected_tool_call_ids:pending,
 collection:req.url==="/batch"?"armed":req.url==="/end"?"collected":"pending"}))});
 s.listen(0,"127.0.0.1",()=>console.log(s.address().port));
 `],{env:{...process.env,CALLS:path.join(dir,"http.jsonl")},stdio:["ignore","pipe","inherit"]});
 try{
 const port=await new Promise((resolve,reject)=>{child.stdout.once("data",b=>resolve(Number(String(b).trim())));child.once("error",reject);child.once("exit",code=>reject(new Error("fixture server exited "+code)))});
 Object.assign(process.env,{ARIES_STEP_OBSERVER_URL:"http://127.0.0.1:"+port,ARIES_STEP_SESSION_ID:"session",ARIES_STEP_TURN_ID:"1",ARIES_STEP_LOG_PATH:path.join(dir,"plugin.jsonl")});
 const hooks={};let service;
 plugin.register({on:(n,f)=>hooks[n]=f,registerService:s=>service=s});
 service.start({stateDir:dir});
 const ctx={sessionKey:"native",runId:"run"};
 const model={runId:"run",callId:"reused",provider:"fixture",model:"fixture"};
 hooks.model_call_started(model,ctx);await new Promise(resolve=>setTimeout(resolve,10));hooks.model_call_ended({...model,durationMs:1},ctx);
 hooks.agent_end({success:true},ctx); // Native context-compaction boundary.
 hooks.model_call_started(model,ctx);await new Promise(resolve=>setTimeout(resolve,10));hooks.model_call_ended({...model,durationMs:2},ctx);
 let reqs=fs.readFileSync(path.join(dir,"http.jsonl"),"utf8").trim().split("\n").map(JSON.parse);
 assert.deepEqual(reqs.map(x=>x.path),["/session/start"]);
 const message={role:"assistant",stopReason:"toolUse",timestamp:123,content:[{type:"toolCall",id:"native-call",name:"exec",arguments:{}}]};
 const original=JSON.stringify(message);
 hooks.before_message_write({message},ctx);
 assert.equal(JSON.stringify(message),original);
 await hooks.before_tool_call({toolCallId:"native-call",toolName:"exec"},ctx);
 hooks.tool_result_persist({toolCallId:"native-call",message:{role:"toolResult"}},ctx);
 hooks.model_call_started(model,ctx);await new Promise(resolve=>setTimeout(resolve,10));hooks.model_call_ended({...model,durationMs:3},ctx);
 await hooks.before_agent_finalize({},ctx);
 hooks.agent_end({success:true},ctx);
 service.stop();
 const data=fs.readFileSync(path.join(dir,"plugin.jsonl"),"utf8");
 const events=data.trim().split("\n").map(JSON.parse);
 assert.equal(events.filter(e=>e.event==="measurement_error").length,0);
 assert.deepEqual(events.filter(e=>e.event==="model_call_started").map(e=>e.model_gap_after_batch_id),[null,null,"llm-step-000001"]);
 const ids=events.filter(e=>e.event==="model_call_started").map(e=>e.model_occurrence_id);
 assert.equal(new Set(ids).size,3);
 assert.deepEqual(ids,events.filter(e=>e.event==="model_call_ended").map(e=>e.model_occurrence_id));
 assert.equal(events.filter(e=>e.event==="terminal_seal"&&e.valid).length,1);
 reqs=fs.readFileSync(path.join(dir,"http.jsonl"),"utf8").trim().split("\n").map(JSON.parse);
 assert.equal(reqs.filter(x=>x.path==="/session/end").length,1);
 assert.equal(reqs.at(-1).body.terminal_complete,true);
 const output=process.env.ARIES_TEST_EVIDENCE;
 if(output){fs.mkdirSync(output,{recursive:true});for(const name of ["plugin.jsonl","http.jsonl"])fs.copyFileSync(path.join(dir,name),path.join(output,name));}
 }finally{child.kill();await new Promise(resolve=>child.once("exit",resolve));fs.rmSync(dir,{recursive:true,force:true});}
});
