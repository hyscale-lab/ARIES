// HTTP transport for synchronous native metadata/persistence barriers.
import fs from "node:fs";
try{
  const {url,payload}=JSON.parse(fs.readFileSync(0,"utf8"));
  const response=await fetch(url,{method:"POST",headers:{"content-type":"application/json"},
    body:JSON.stringify(payload),signal:AbortSignal.timeout(120000)});
  const ack=await response.json();
  if(!response.ok||ack.ok!==true)process.exitCode=1;
  else process.stdout.write(JSON.stringify(ack)+"\n");
}catch{process.exitCode=1;}
