import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {spawn} from "node:child_process";
import plugin from "./index.mjs";

async function fixture(t, overrides={}) {
  const directory=fs.mkdtempSync(path.join(os.tmpdir(),"aries-plugin-identity-"));
  const child=spawn(process.execPath,["--input-type=module","-e",`
    import http from "node:http";
    import fs from "node:fs";
    const overrides=JSON.parse(process.env.OVERRIDES);
    let pending=new Set();
    const server=http.createServer(async(req,res)=>{
      let body=""; for await(const chunk of req)body+=chunk;
      const value=JSON.parse(body);
      fs.appendFileSync(process.env.CALLS,JSON.stringify({route:req.url,...value})+"\\n");
      let ack={ok:true,session_id:value.session_id,turn_id:value.turn_id};
      if(req.url==="/session/start")ack.paused=true;
      else if(req.url==="/session/end")Object.assign(ack,{released:true,paused:false,terminal_complete:value.terminal_complete});
      else {
        if(req.url==="/batch")pending=new Set(value.expected_tool_call_ids);
        if(req.url==="/end")pending.delete(value.tool_call_id);
        Object.assign(ack,{batch_id:value.batch_id,llm_step_id:value.llm_step_id,
          collection:req.url==="/batch"?"armed":pending.size?"pending":"collected",
          remaining_expected_tool_call_ids:[...pending]});
      }
      Object.assign(ack,overrides[req.url]??{});
      res.writeHead(200,{"content-type":"application/json"});res.end(JSON.stringify(ack));
    });
    server.listen(0,"127.0.0.1",()=>console.log(server.address().port));
  `],{env:{...process.env,OVERRIDES:JSON.stringify(overrides),CALLS:path.join(directory,"calls.jsonl")},stdio:["ignore","pipe","inherit"]});
  const port=await new Promise((resolve,reject)=>{child.stdout.once("data",data=>resolve(Number(data.toString().trim())));child.once("error",reject);});
  const env={...process.env};
  Object.assign(process.env,{ARIES_STEP_OBSERVER_URL:"http://127.0.0.1:"+port,
    ARIES_STEP_SESSION_ID:"task",ARIES_STEP_TURN_ID:"1",ARIES_STEP_LOG_PATH:path.join(directory,"plugin.jsonl")});
  delete globalThis[Symbol.for("aries.step.pages.service.v1")];
  const hooks={},errors=[];let service;
  plugin.register({on:(name,handler)=>hooks[name]=handler,registerService:value=>service=value,
    logger:{error:value=>errors.push(value)}});
  t.after(async()=>{
    service.stop();
    const exited=new Promise(resolve=>child.once("exit",resolve));child.kill();await exited;
    for(const name of Object.keys(process.env))if(!(name in env))delete process.env[name];
    Object.assign(process.env,env);
    delete globalThis[Symbol.for("aries.step.pages.service.v1")];
    fs.rmSync(directory,{recursive:true,force:true});
  });
  const ctx={sessionKey:"native",sessionId:"native-id",runId:"run"};
  const rows=()=>fs.readFileSync(path.join(directory,"plugin.jsonl"),"utf8").trim().split("\n").map(JSON.parse);
  return {hooks,service,ctx,errors,rows,
    start:()=>service.start({stateDir:directory}),
    calls:()=>fs.readFileSync(path.join(directory,"calls.jsonl"),"utf8").trim().split("\n").map(JSON.parse),
    declare(ids=["a"],messageId="assistant") {
      hooks.before_message_write({message:{role:"assistant",stopReason:"toolUse",id:messageId,
        content:ids.map(id=>({type:"toolCall",id,name:"exec"}))}},ctx);
    },
    end(id="a",message={}) {hooks.tool_result_persist({toolCallId:id,message},ctx);},
    model() {hooks.model_call_started({runId:"run",callId:"model"},ctx);hooks.model_call_ended({runId:"run",callId:"model"},ctx);},
    finalize(context=ctx) {return hooks.before_agent_finalize({},context);},
  };
}

test("a successful pause acknowledgement must identify the same session and held VM",async t=>{
  const f=await fixture(t,{"/session/start":{session_id:"other",paused:false}});
  assert.throws(f.start,/initial guest pause not confirmed/);
});

test("a stale batch acknowledgement cannot certify batch registration",async t=>{
  const f=await fixture(t,{"/batch":{batch_id:"another-batch"}});
  f.start();f.model();f.declare();
  assert.equal(f.rows().filter(row=>row.event==="batch_confirmed").length,0);
  assert.ok(f.rows().some(row=>row.reason==="batch_registration_failed"));
});

test("a premature collection acknowledgement cannot finish missing native results",async t=>{
  const f=await fixture(t,{"/end":{collection:"collected",remaining_expected_tool_call_ids:[]}});
  f.start();f.model();f.declare(["a","b"]);f.end("a");await f.finalize();
  assert.equal(f.rows().filter(row=>row.event==="batch_completed").length,0);
  assert.equal(f.rows().find(row=>row.event==="terminal_seal").valid,false);
  assert.equal(f.calls().find(row=>row.route==="/session/end").terminal_complete,false);
});

test("a different result message ID cannot satisfy a declared native call",async t=>{
  const f=await fixture(t);
  f.start();f.model();f.declare();f.end("a",{toolCallId:"other"});await f.finalize();
  assert.equal(f.calls().filter(row=>row.route==="/end").length,0);
  assert.equal(f.rows().find(row=>row.event==="terminal_seal").valid,false);
});

test("a terminal hook for another native session cannot release this session",async t=>{
  const f=await fixture(t);
  f.start();f.model();await f.finalize({...f.ctx,sessionKey:"another-native-session"});
  assert.equal(f.calls().filter(row=>row.route==="/session/end").length,0);
});

test("distinct assistant messages remain distinct when a completed native call ID is reused",async t=>{
  const f=await fixture(t);
  f.start();f.model();f.declare(["a"],"first");f.end();f.declare(["a"],"second");f.end();
  assert.equal(f.calls().filter(row=>row.route==="/batch").length,2);
  assert.equal(f.rows().filter(row=>row.event==="batch_completed").length,2);
});

test("an unreleased terminal acknowledgement is an explicit measurement failure",async t=>{
  const f=await fixture(t,{"/session/end":{released:false,paused:true}});
  f.start();f.model();await f.finalize();
  assert.equal(f.rows().filter(row=>row.event==="session_resume_confirmed").length,0);
  assert.ok(f.rows().some(row=>row.reason==="terminal_resume_failed"));
  assert.ok(f.errors.length>0);
});

test("a begin acknowledgement for another batch is rejected",async t=>{
  const f=await fixture(t,{"/begin":{llm_step_id:"other-batch"}});
  f.start();f.model();f.declare();
  await f.hooks.before_tool_call({toolCallId:"a",toolName:"exec"},f.ctx);
  assert.equal(f.rows().filter(row=>row.event==="begin_confirmed").length,0);
  assert.ok(f.rows().some(row=>row.reason==="begin_failed"));
});

test("a begin timeout stays explicit without retries or changing native tool parameters",async t=>{
  const f=await fixture(t);
  f.start();f.model();f.declare();
  const mocked=t.mock.method(globalThis,"fetch",async(_url,options)=>{
    assert.ok(options.signal instanceof AbortSignal);
    throw new DOMException("fixture timeout","TimeoutError");
  });
  const event={toolCallId:"a",toolName:"exec",params:{command:"native command remains unchanged"}};
  const original=structuredClone(event);
  assert.equal(await f.hooks.before_tool_call(event,f.ctx),undefined);
  assert.deepEqual(event,original);
  assert.equal(mocked.mock.callCount(),1);
  assert.ok(f.rows().some(row=>row.reason==="begin_failed" && row.error_type==="TimeoutError"));
  assert.ok(f.errors.some(message=>message.includes("begin_failed")));
});

test("conflicting native context and declared tool names cannot admit a call",async t=>{
  const f=await fixture(t);
  f.start();f.model();f.declare();
  await f.hooks.before_tool_call({toolCallId:"a",toolName:"another-tool"},f.ctx);
  assert.equal(f.calls().filter(row=>row.route==="/begin").length,0);
  assert.ok(f.rows().some(row=>row.reason==="native_call_identity_mismatch"));
});

test("missing native model IDs cannot certify a completed terminal horizon",async t=>{
  const f=await fixture(t);
  f.start();
  f.hooks.model_call_started({runId:"run"},f.ctx);
  f.hooks.model_call_ended({runId:"run"},f.ctx);
  await f.finalize();
  assert.equal(f.rows().find(row=>row.event==="terminal_seal").valid,false);
  assert.ok(f.rows().some(row=>row.reason==="invalid_native_model_identity"));
});
