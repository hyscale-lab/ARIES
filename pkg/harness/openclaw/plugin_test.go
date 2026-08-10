package openclaw

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func nodeCommand(t *testing.T, arguments ...string) *exec.Cmd {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for OpenClaw plugin client tests")
	}
	return exec.Command(node, arguments...)
}

func pluginTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access.token")
	if err := os.WriteFile(path, []byte("task-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeNodeModule(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "preload.mjs")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestE2BHelperDecodesNDJSONAndPropagatesExitCode(t *testing.T) {
	tokenFile := pluginTokenFile(t)
	record := filepath.Join(t.TempDir(), "request.json")
	preload := writeNodeModule(t, `
import fs from "node:fs";
globalThis.fetch=async (url,options)=>{
  fs.writeFileSync(process.env.RECORD,JSON.stringify({url,headers:Object.fromEntries(options.headers.entries()),body:JSON.parse(options.body)}));
  const lines=[{"event":{"start":{"pid":123}}},{"event":{"data":{"stdout":"AP9vdXQ="}}},{"event":{"data":{"stderr":"/gBlcnI="}}},{"event":{"end":{"exitCode":7,"error":null}}}];
  const encoder=new TextEncoder();
  return new Response(new ReadableStream({start(controller){for(const line of lines)controller.enqueue(encoder.encode(JSON.stringify(line)+"\n"));controller.close();}}),{status:200});
};`)
	helper := filepath.Join("assets", "aries-e2b", "helper.mjs")
	command := nodeCommand(t, "--import", preload, helper, "exec", "printf command", "/workspace", `{"A":"1"}`)
	command.Env = append(os.Environ(), "ARIES_E2B_ADDRESS=http://172.30.0.1:43123", "ARIES_E2B_SANDBOX_ID=sandbox-1", "ARIES_E2B_TOKEN_FILE="+tokenFile, "RECORD="+record)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("helper exit=%v stdout=%v stderr=%v", err, stdout.Bytes(), stderr.Bytes())
	}
	if !bytes.Equal(stdout.Bytes(), []byte{0x00, 0xff, 'o', 'u', 't'}) || !bytes.Equal(stderr.Bytes(), []byte{0xfe, 0x00, 'e', 'r', 'r'}) {
		t.Fatalf("decoded stdout=%v stderr=%v", stdout.Bytes(), stderr.Bytes())
	}
	recorded, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    struct {
			Process struct {
				Cmd  string            `json:"cmd"`
				Args []string          `json:"args"`
				Cwd  string            `json:"cwd"`
				Envs map[string]string `json:"envs"`
			} `json:"process"`
		} `json:"body"`
	}
	if err := json.Unmarshal(recorded, &request); err != nil || request.URL != "http://172.30.0.1:43123/v1/process/start" || request.Headers["e2b-sandbox-id"] != "sandbox-1" || request.Headers["x-access-token"] != "task-token" || request.Body.Process.Cmd != "/bin/bash" || !reflect.DeepEqual(request.Body.Process.Args, []string{"-lc", "printf command", "aries-e2b"}) || request.Body.Process.Cwd != "/workspace" || request.Body.Process.Envs["A"] != "1" {
		t.Fatalf("recorded request=%#v err=%v", request, err)
	}
}

func TestE2BHelperTerminationCancelsAttachedRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal behavior")
	}
	tokenFile := pluginTokenFile(t)
	started := filepath.Join(t.TempDir(), "started")
	canceled := filepath.Join(t.TempDir(), "canceled")
	preload := writeNodeModule(t, `
import fs from "node:fs";
globalThis.fetch=async (_url,options)=>new Promise((_resolve,reject)=>{
  fs.writeFileSync(process.env.STARTED,"1");
  const keepalive=setInterval(()=>{},1000);
  options.signal.addEventListener("abort",()=>{clearInterval(keepalive);fs.writeFileSync(process.env.CANCELED,"1");reject(new DOMException("aborted","AbortError"));},{once:true});
});`)
	command := nodeCommand(t, "--import", preload, filepath.Join("assets", "aries-e2b", "helper.mjs"), "exec", "sleep", "/workspace", `{}`)
	command.Env = append(os.Environ(), "ARIES_E2B_ADDRESS=http://172.30.0.1:43123", "ARIES_E2B_SANDBOX_ID=sandbox-1", "ARIES_E2B_TOKEN_FILE="+tokenFile, "STARTED="+started, "CANCELED="+canceled)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper request did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := command.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 143 {
		t.Fatalf("helper cancellation exit=%v", err)
	}
	if _, err := os.Stat(canceled); err != nil {
		t.Fatalf("helper termination did not cancel HTTP request: %v", err)
	}
}

func TestE2BClientRunShellAndEveryPinnedFilesystemMethod(t *testing.T) {
	tokenFile := pluginTokenFile(t)
	clientPath, err := filepath.Abs(filepath.Join("assets", "aries-e2b", "client.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	script := `
import {pathToFileURL} from "node:url";
const {createAriesClient,createAriesFsBridge,runAriesShellCommand}=await import(pathToFileURL(process.env.CLIENT));
const seen={};
globalThis.fetch=async (url,options={})=>{
 const route=new URL(url).pathname,key=(options.method??"GET")+" "+route;seen[key]=(seen[key]??0)+1;
 if(options.headers.get("E2b-Sandbox-Id")!=="sandbox-1"||options.headers.get("X-Access-Token")!=="task-token")throw Error("auth");
 if(route==="/v1/process/start")return new Response(['{"event":{"start":{"pid":1}}}','{"event":{"data":{"stdout":"b3V0"}}}','{"event":{"data":{"stderr":"ZXJy"}}}','{"event":{"end":{"exitCode":4,"error":null}}}'].join("\n")+"\n",{status:200});
 if(route==="/v1/files"&&(options.method??"GET")==="GET")return new Response(new Uint8Array([0,255]),{status:200});
 if(route==="/v1/files"&&options.method==="POST"){if(Buffer.from(await new Response(options.body).arrayBuffer()).toString()!=="written")throw Error("write body");return new Response('{"ok":true}',{status:200});}
 if(route==="/v1/filesystem/stat")return new Response('{"entry":{"path":"/workspace/file","name":"file","type":"file","size":2,"mode":"0644","modifiedAt":"1970-01-01T00:00:01Z"}}',{status:200});
 return new Response('{"ok":true}',{status:200});
};
const client=createAriesClient({address:process.env.ADDRESS,sandboxId:"sandbox-1",tokenFile:process.env.TOKEN});
const shell=await runAriesShellCommand(client,"/workspace",{script:"exit 4",args:["arg"],allowFailure:true});
if(shell.code!==4||shell.stdout.toString()!=="out"||shell.stderr.toString()!=="err")throw Error("shell result");
const bridge=createAriesFsBridge(client,"/workspace");
const resolved=bridge.resolvePath({filePath:"file"});if(resolved.containerPath!=="/workspace/file")throw Error("resolve");
const read=await bridge.readFile({filePath:"file"});if(read.length!==2||read[1]!==255)throw Error("read");
await bridge.writeFile({filePath:"file",data:"written"});
await bridge.mkdirp({filePath:"dir"});await bridge.remove({filePath:"dir",recursive:true,force:true});
await bridge.rename({from:"file",to:"renamed"});
const stat=await bridge.stat({filePath:"file"});if(stat.type!=="file"||stat.size!==2||stat.mtimeMs!==1000)throw Error("stat");
for(const route of ["POST /v1/process/start","GET /v1/files","POST /v1/files","POST /v1/filesystem/make-dir","POST /v1/filesystem/remove","POST /v1/filesystem/move","POST /v1/filesystem/stat"])if(seen[route]!==1)throw Error("missing "+route);
`
	command := nodeCommand(t, "--input-type=module", "-e", script)
	command.Env = append(os.Environ(), "CLIENT="+clientPath, "ADDRESS=http://172.30.0.1:43123", "TOKEN="+tokenFile)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("client test: %v: %s", err, output)
	}
}
