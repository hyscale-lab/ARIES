import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const helper = fileURLToPath(new URL("./end-request.mjs", import.meta.url));
const stamp = () => ({plugin_pid:process.pid, plugin_monotonic_ns:process.hrtime.bigint().toString(), plugin_timestamp_ms:Date.now()});
export default {
  id:"aries-step-pages", name:"ARIES LLM-step page observation",
  register(api) {
    // Native scoped registry reloads share the service's process state.
    const symbol=Symbol.for("aries.step.pages.service.v1");
    const shared=globalThis[symbol] ??= {active:new Map(), registrations:0};
    shared.batches ??= new Map();
    shared.calls ??= new Map();
    shared.declarations ??= new Map();
    shared.batchSequence ??= 0;
    shared.modelPending ??= new Map();
    shared.modelSequence ??= 0;
    shared.lastCompletedBatch ??= null;
    shared.modelCompleted ??= new Set();
    shared.measurementErrors ??= 0;
    const registration=++shared.registrations;
    const key=(session,id)=>JSON.stringify([session ?? "",id]);
    const record=(event,fields={})=>{
      if(shared.fd===undefined)throw new Error("LLM-step observer service not started");
      fs.writeSync(shared.fd,JSON.stringify({event,registration,...stamp(),...fields})+"\n");
    };
    const failure=(reason,fields={})=>{
      shared.measurementErrors++;
      try{record("measurement_error",{reason,...fields});}
      catch{api.logger?.error?.("ARIES LLM-step observation log unavailable");}
    };
    const syncRequest=(route,payload)=>{
      const result=spawnSync(process.execPath,[helper],{
        input:JSON.stringify({url:shared.endpoint+route,payload}),
        encoding:"utf8",timeout:125000,maxBuffer:65536,env:{PATH:process.env.PATH},
      });
      if(result.status!==0 || result.error)return {ok:false,helper_exit:result.status,error_type:result.error?.name};
      try{
        const ack=JSON.parse(result.stdout);
        return ack.ok===true ? {ok:true,ack} : {ok:false,error_type:"InvalidAcknowledgement"};
      }catch{return {ok:false,error_type:"InvalidAcknowledgement"};}
    };
    const perCall=(batch,id,name,ctx,event={})=>({
      ...shared.hierarchy,batch_id:batch.metadata.batch_id,llm_step_id:batch.metadata.llm_step_id,
      run_id:event.runId ?? ctx.runId ?? batch.runId,
      native_session_id:ctx.sessionId,native_session_key:ctx.sessionKey,
      tool_call_id:id,tool_name:name,
    });
    api.registerService({
      id:"aries-step-pages",
      start(ctx){
        if(shared.fd!==undefined){record("service_start_reused");return;}
        const raw=process.env.ARIES_STEP_OBSERVER_URL;
        if(!raw || !process.env.ARIES_STEP_SESSION_ID || !process.env.ARIES_STEP_TURN_ID)throw new Error("LLM-step observer endpoint/session/turn required");
        const url=new URL(raw);
        if(!["http:","https:"].includes(url.protocol)||url.username||url.password||url.search||url.hash)throw new Error("invalid LLM-step observer endpoint");
        shared.endpoint=raw.replace(/\/$/,"");
        shared.hierarchy={session_id:process.env.ARIES_STEP_SESSION_ID,turn_id:process.env.ARIES_STEP_TURN_ID};
        const target=process.env.ARIES_STEP_LOG_PATH || path.join(ctx.stateDir,"aries-step-pages.jsonl");
        fs.mkdirSync(path.dirname(target),{recursive:true,mode:0o700});
        shared.fd=fs.openSync(target,"wx",0o600);
        record("service_started",shared.hierarchy);
        record("session_pause_requested",shared.hierarchy);
        const init=syncRequest("/session/start",{...shared.hierarchy,...stamp()});
        if(!init.ok){failure("initial_session_pause_failed",init);throw new Error("initial guest pause not confirmed");}
        record("session_pause_confirmed",{...shared.hierarchy,observer_ack:init.ack});
      },
      stop(){
        if(shared.fd===undefined)return;
        // Container removal is followed by the host-owned abort/release barrier.
        // Service teardown itself is not evidence of a completed model horizon.
        for(const metadata of shared.active.values())failure("unfinished_native_call",metadata);
        for(const batch of shared.batches.values())if(!batch.complete)failure("unfinished_llm_batch",batch.metadata);
        record("service_stopped",{unfinished_steps:shared.active.size,unfinished_batches:[...shared.batches.values()].filter(b=>!b.complete).length});
        fs.closeSync(shared.fd);shared.fd=undefined;shared.endpoint=undefined;shared.hierarchy=undefined;
        shared.active.clear();shared.batches.clear();shared.calls.clear();shared.declarations.clear();
        shared.modelPending.clear();shared.modelCompleted.clear();shared.terminalSealed=false;
        // Sequences and prior errors are process-lifetime provenance, never recycled on reload.
      },
    });
    const releaseSession=(reason,terminal_complete=false)=>{
      if(!shared.endpoint)return;
      record("session_resume_requested",{reason,...shared.hierarchy});
      const result=syncRequest("/session/end",{...shared.hierarchy,...stamp(),reason,terminal_complete});
      if(!result.ok)failure("terminal_resume_failed",{reason,...result});
      else record("session_resume_confirmed",{reason,observer_ack:result.ack});
    };
    api.on("before_agent_finalize",async()=>{
      const begin=process.hrtime.bigint();
      const deadline=Date.now()+1000;
      while(shared.modelPending.size && Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,10));
      const unfinished=[...shared.batches.values()].filter(b=>!b.complete).length;
      const valid=shared.modelPending.size===0 && shared.modelCompleted.size>0 &&
        shared.active.size===0 && unfinished===0 && shared.measurementErrors===0;
      if(!valid)failure("terminal_seal_incomplete");
      shared.terminalSealed=true;
      record("terminal_seal",{valid,pending_model_calls:shared.modelPending.size,
        completed_model_calls:shared.modelCompleted.size,unfinished_batches:unfinished,
        active_tool_calls:shared.active.size,measurement_errors:shared.measurementErrors,
        wait_limit_ms:1000,wait_elapsed_ns:(process.hrtime.bigint()-begin).toString(),
        horizon:"awaited successful-agent terminal hook; export occurs before container stop"});
      fs.fsyncSync(shared.fd);
      releaseSession("before_agent_finalize",valid);
      fs.fsyncSync(shared.fd);
    },{timeoutMs:125000});
    api.on("agent_end",(event)=>{
      // Native context compaction emits agent_end before continuing this session.
      // Only before_agent_finalize or the host's post-stop barrier may release it.
      record("agent_end_observed",{...shared.hierarchy,success:event.success===true,
        terminal_sealed:shared.terminalSealed===true,session_release_performed:false});
    });
    for(const hook of ["model_call_started","model_call_ended"]){
      api.on(hook,(event,ctx)=>{
        const fields={...shared.hierarchy,native_session_key:ctx.sessionKey,
          run_id:event.runId,call_id:event.callId,provider:event.provider,model:event.model};
        for(const k of ["durationMs","outcome","errorCategory","failureKind","timeToFirstByteMs","requestPayloadBytes","responseStreamBytes"]){
          if(event[k]!==undefined)fields[k]=event[k];
        }
        const id=key(event.runId,event.callId);
        if(shared.terminalSealed)failure("model_callback_after_terminal_seal",{hook,call_id:event.callId});
        if(hook==="model_call_started"){
          if(shared.modelPending.size)failure("overlapping_model_start",{call_id:event.callId});
          if([...shared.batches.values()].some(b=>!b.complete))failure("model_start_with_unfinished_batch",{call_id:event.callId});
          if(shared.modelPending.has(id)){
            failure("duplicate_model_start",{call_id:event.callId});
            return;
          }
          const occurrence={model_occurrence_id:"model-occurrence-"+String(++shared.modelSequence).padStart(6,"0"),
            model_gap_after_batch_id:shared.lastCompletedBatch};
          shared.modelPending.set(id,occurrence);
          Object.assign(fields,occurrence);
        }else{
          const occurrence=shared.modelPending.get(id);
          if(!occurrence){failure("model_end_without_start",{call_id:event.callId});return;}
          shared.modelPending.delete(id);
          shared.modelCompleted.add(occurrence.model_occurrence_id);
          Object.assign(fields,occurrence);
        }
        record(hook,fields);
      });
    }
    api.on("before_message_write",(event,ctx)=>{
      const message=event.message;
      if(message?.role!=="assistant" || message.stopReason!=="toolUse")return;
      const calls=Array.isArray(message.content) ? message.content.filter(c=>c?.type==="toolCall") : [];
      if(calls.length===0)return;
      if(!shared.endpoint){failure("batch_missing_service");return;}
      if(calls.some(c=>typeof c.id!=="string"||!c.id||typeof c.name!=="string"||!c.name) || new Set(calls.map(c=>c.id)).size!==calls.length){
        failure("invalid_native_batch_identity");return;
      }
      const requested_tools=calls.map(c=>({tool_call_id:c.id,tool_name:c.name}));
      const fingerprint=JSON.stringify([ctx.sessionKey ?? "",message.timestamp ?? null,calls.map(c=>c.id)]);
      const previous=shared.declarations.get(fingerprint);
      if(previous){record("batch_declaration_reused",{batch_id:previous,llm_step_id:previous});return;}
      const batch_id="llm-step-"+String(++shared.batchSequence).padStart(6,"0");
      const metadata={
        ...shared.hierarchy,batch_id,llm_step_id:batch_id,native_session_key:ctx.sessionKey,
        expected_tool_call_ids:calls.map(c=>c.id),requested_tools,
        native_assistant_message_id:typeof message.id==="string" ? message.id : typeof message.responseId==="string" ? message.responseId : null,
        native_assistant_timestamp_ms:typeof message.timestamp==="number" ? message.timestamp : null,
        native_assistant_provider:message.provider,native_assistant_model:message.model,
        native_assistant_stop_reason:message.stopReason,
      };
      const batch={metadata,complete:false,completed:new Set(),registered:false};
      shared.batches.set(batch_id,batch);shared.declarations.set(fingerprint,batch_id);
      for(const call of requested_tools){
        const k=key(ctx.sessionKey,call.tool_call_id);
        const prior=shared.calls.get(k);
        if(prior && !prior.batch.complete){failure("native_call_id_reused_before_completion",{batch_id,tool_call_id:call.tool_call_id});continue;}
        shared.calls.set(k,{batch,tool_name:call.tool_name});
      }
      record("batch_requested",metadata);
      const result=syncRequest("/batch",{...metadata,...stamp()});
      if(!result.ok)failure("batch_registration_failed",{...metadata,...result});
      else{batch.registered=true;record("batch_confirmed",{...metadata,observer_ack:result.ack});}
      // Metadata observation only: never replace, reject, or mutate the message.
    });
    api.on("before_tool_call",async(event,ctx)=>{
      const id=event.toolCallId ?? ctx.toolCallId;
      const entry=shared.calls.get(key(ctx.sessionKey,id));
      if(!entry || !shared.endpoint){failure("native_call_without_declared_batch",{tool_call_id:id,native_session_key:ctx.sessionKey});return;}
      const {batch}=entry;
      const k=key(ctx.sessionKey,id);
      if(shared.active.has(k)||batch.completed.has(id)){failure("duplicate_native_begin",{batch_id:batch.metadata.batch_id,tool_call_id:id});return;}
      batch.runId ??= event.runId ?? ctx.runId;
      const metadata=perCall(batch,id,event.toolName ?? ctx.toolName ?? entry.tool_name,ctx,event);
      shared.active.set(k,metadata);record("begin_requested",metadata);
      try{
        const response=await fetch(shared.endpoint+"/begin",{method:"POST",headers:{"content-type":"application/json"},body:JSON.stringify({...metadata,...stamp()}),signal:AbortSignal.timeout(120000)});
        const ack=await response.json();
        if(!response.ok||ack.ok!==true)throw new Error("invalid begin acknowledgement");
        record("begin_confirmed",{...metadata,observer_ack:ack});
      }catch(error){failure("begin_failed",{...metadata,error_type:error?.name ?? "Error"});}
    },{timeoutMs:125000});
    api.on("tool_result_persist",(event,ctx)=>{
      const id=event.toolCallId ?? ctx.toolCallId;
      const k=key(ctx.sessionKey,id);
      const entry=shared.calls.get(k);
      if(event.isSynthetic===true){failure("synthetic_result",{tool_call_id:id,native_session_key:ctx.sessionKey});return;}
      if(!entry){failure("result_without_declared_batch",{tool_call_id:id,native_session_key:ctx.sessionKey});return;}
      const {batch}=entry;
      if(batch.completed.has(id)){failure("duplicate_native_result",{batch_id:batch.metadata.batch_id,tool_call_id:id});return;}
      // Native argument validation and missing-tool results may legitimately
      // bypass before_tool_call. The assistant-declared expected ID owns them.
      const metadata=shared.active.get(k) ?? perCall(batch,id,event.toolName ?? entry.tool_name,ctx);
      const begin_observed=shared.active.has(k);
      record("end_requested",{...metadata,begin_observed});
      const result=syncRequest("/end",{...metadata,...stamp(),is_error:event.message?.isError===true,begin_observed});
      if(!result.ok)failure("end_failed",{...metadata,...result});
      else{
        batch.completed.add(id);
        record("end_confirmed",{...metadata,begin_observed,observer_ack:result.ack});
        if(result.ack.collection==="collected"){
          batch.complete=true;shared.lastCompletedBatch=batch.metadata.batch_id;record("batch_completed",{...batch.metadata,observer_ack:result.ack});
        }
      }
      shared.active.delete(k);
      // Undefined preserves the exact native tool-result message.
    });
  },
};
