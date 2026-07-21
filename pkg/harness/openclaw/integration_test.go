//go:build integration

package openclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const fakeModelPrefix = "aries-m5-fake-model-"

type fakeModelResource struct {
	Name      string
	ID        string
	AttemptID string
	Tentative bool
	Owned     bool
}

type fakeModelProofDocker struct {
	attemptID      string
	hiddenInspects int
	hideRetainedID bool
	foreign        bool
	commands       [][]string
}

func (fake *fakeModelProofDocker) Run(_ context.Context, _ []byte, args ...string) (commandResult, error) {
	fake.commands = append(fake.commands, append([]string(nil), args...))
	if len(args) != 3 || args[0] != "container" || args[1] != "inspect" {
		return commandResult{exitCode: 125, stderr: []byte("unexpected command")}, errors.New("unexpected command")
	}
	if fake.hiddenInspects > 0 {
		fake.hiddenInspects--
		return commandResult{exitCode: 1, stderr: []byte("No such container")}, errors.New("missing")
	}
	if fake.hideRetainedID && args[2] == "fake-id" {
		return commandResult{exitCode: 1, stderr: []byte("No such container")}, errors.New("missing")
	}
	attemptID := fake.attemptID
	if fake.foreign {
		attemptID = "foreign-attempt"
	}
	inspection := containerInspection{ID: "fake-id"}
	inspection.Config.Labels = map[string]string{
		"aries.managed": "true", "aries.milestone": "m5", "aries.kind": "fake-model", "aries.attempt": attemptID,
	}
	content, _ := json.Marshal([]containerInspection{inspection})
	return commandResult{stdout: content}, nil
}

func TestFakeModelOwnershipProofRetriesDelayedVisibility(t *testing.T) {
	for name, test := range map[string]struct {
		resource *fakeModelResource
		fake     *fakeModelProofDocker
		preferID bool
		want     ownershipState
	}{
		"initial name": {
			resource: &fakeModelResource{Name: "fake-name", AttemptID: "attempt", Tentative: true},
			fake:     &fakeModelProofDocker{attemptID: "attempt", hiddenInspects: 1},
			want:     ownershipOwned,
		},
		"cleanup retained ID then name": {
			resource: &fakeModelResource{Name: "fake-name", ID: "fake-id", AttemptID: "attempt", Tentative: true},
			fake:     &fakeModelProofDocker{attemptID: "attempt", hideRetainedID: true},
			preferID: true,
			want:     ownershipOwned,
		},
		"delayed foreign collision": {
			resource: &fakeModelResource{Name: "fake-name", AttemptID: "attempt", Tentative: true},
			fake:     &fakeModelProofDocker{attemptID: "attempt", hiddenInspects: 1, foreign: true},
			want:     ownershipForeign,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			state, _, err := awaitFakeModelOwnership(ctx, test.fake, test.resource, test.preferID)
			if state != test.want {
				t.Fatalf("ownership state = %v, want %v; err=%v", state, test.want, err)
			}
			if state == ownershipOwned && err != nil {
				t.Fatalf("owned proof returned error: %v", err)
			}
			for _, command := range test.fake.commands {
				if len(command) < 2 || command[0] != "container" || command[1] != "inspect" {
					t.Fatalf("ownership proof issued destructive command: %q", command)
				}
			}
		})
	}
}

type gitSnapshot struct {
	Head   string `json:"head"`
	Status string `json:"status"`
	Reflog string `json:"reflog"`
	Log    string `json:"log"`
	Files  string `json:"files"`
}

func splitEvidence(content string) map[string]string {
	markers := []string{"ARIES_HEAD", "ARIES_STATUS", "ARIES_REFLOG", "ARIES_LOG", "ARIES_FILES"}
	result := make(map[string]string)
	for index, marker := range markers {
		start := strings.Index(content, marker+"\n")
		if start < 0 {
			continue
		}
		start += len(marker) + 1
		end := len(content)
		if index+1 < len(markers) {
			if next := strings.Index(content[start:], markers[index+1]+"\n"); next >= 0 {
				end = start + next
			}
		}
		result[marker] = strings.TrimSpace(content[start:end])
	}
	return result
}

func startFakeModel(ctx context.Context, network, keyHash, evidenceDir string) (fakeModelResource, error) {
	attemptID, err := randomID()
	if err != nil {
		return fakeModelResource{}, err
	}
	resource := fakeModelResource{Name: fakeModelPrefix + attemptID, AttemptID: attemptID, Tentative: true}
	script := fakeModelScript()
	create := integrationDocker(ctx, nil,
		"container", "create", "--name", resource.Name,
		"--label", managedLabel, "--label", milestoneLabel, "--label", "aries.kind=fake-model", "--label", "aries.attempt="+attemptID,
		"--network", network, "--network-alias", "fake-model",
		"--user", fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid()),
		"--no-healthcheck",
		"--mount", "type=bind,src="+evidenceDir+",dst=/evidence",
		"--entrypoint", "node", PinnedImage,
		"-e", script, keyHash,
	)
	createdID := strings.TrimSpace(string(create.stdout))
	if createdID != "" {
		resource.ID = createdID
	}
	fail := func(primary error) (fakeModelResource, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		cleanupErr := removeOwnedFakeModel(cleanupCtx, &resource)
		if cleanupErr != nil {
			return resource, errors.Join(primary, cleanupErr)
		}
		return fakeModelResource{}, primary
	}
	proofCtx, proofCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	state, inspection, inspectErr := awaitFakeModelOwnership(proofCtx, execRunner{binary: "docker"}, &resource, false)
	proofCancel()
	applyFakeModelOwnership(&resource, state, inspection)
	if inspectErr != nil {
		return fail(errors.Join(fmt.Errorf("create fake model: %s", create.stderr), inspectErr))
	}
	if state != ownershipOwned {
		if create.exitCode != 0 {
			return fail(fmt.Errorf("create fake model without owned result: %s", create.stderr))
		}
		return fail(errors.New("created fake model lacks the exact attempt ownership labels"))
	}
	if create.exitCode != 0 {
		return fail(fmt.Errorf("create fake model ambiguously: %s", create.stderr))
	}
	if createdID == "" || createdID != resource.ID {
		return fail(fmt.Errorf("fake model create returned ID %q, inspected %q", createdID, resource.ID))
	}
	wantedUser := fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid())
	healthcheckDisabled := inspection.Config.Healthcheck != nil && equalStrings(inspection.Config.Healthcheck.Test, []string{"NONE"})
	if inspection.ID != resource.ID || inspection.Config.Image != PinnedImage || inspection.Config.User != wantedUser || !healthcheckDisabled || len(inspection.Mounts) != 1 || inspection.Mounts[0].Type != "bind" || inspection.Mounts[0].Source != evidenceDir || inspection.Mounts[0].Destination != "/evidence" || !inspection.Mounts[0].RW {
		return fail(fmt.Errorf("fake model ID/image/user/health/labels/mounts = %q/%q/%q/%#v/%#v/%#v", inspection.ID, inspection.Config.Image, inspection.Config.User, inspection.Config.Healthcheck, inspection.Config.Labels, inspection.Mounts))
	}
	if len(inspection.NetworkSettings.Networks) != 1 {
		return fail(errors.New("fake model is not attached to exactly one task-local network"))
	}
	if _, ok := inspection.NetworkSettings.Networks[network]; !ok {
		return fail(errors.New("fake model is not attached to the task-local network"))
	}
	if start := integrationDocker(ctx, nil, "container", "start", resource.ID); start.exitCode != 0 {
		return fail(fmt.Errorf("start fake model: %s", start.stderr))
	}
	readyPath := filepath.Join(evidenceDir, "ready")
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	defer readyCancel()
	deadline := time.NewTicker(50 * time.Millisecond)
	defer deadline.Stop()
	for {
		content, err := readStablePrivateFile(readyPath, 0o600)
		if err == nil && bytes.Equal(content, []byte("ready")) {
			clear(content)
			return resource, nil
		}
		clear(content)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(err)
		}
		select {
		case <-readyCtx.Done():
			return fail(fmt.Errorf("await fake model ready evidence: %w", readyCtx.Err()))
		case <-deadline.C:
		}
	}
}

func awaitFakeModelOwnership(ctx context.Context, cli commandRunner, resource *fakeModelResource, preferRetainedID bool) (ownershipState, containerInspection, error) {
	references := []string{resource.Name}
	if preferRetainedID && resource.ID != "" {
		references = []string{resource.ID, resource.Name}
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	lastState := ownershipUnknown
	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastState == ownershipAbsent {
				return ownershipAbsent, containerInspection{}, nil
			}
			return ownershipUnknown, containerInspection{}, errors.Join(lastErr, ctx.Err())
		}
		state, inspection, err := inspectFakeModelOwnershipReferences(ctx, cli, resource, references)
		if state == ownershipOwned || state == ownershipForeign {
			return state, inspection, err
		}
		lastState = state
		lastErr = err
		select {
		case <-ctx.Done():
			if lastState == ownershipAbsent {
				return ownershipAbsent, containerInspection{}, nil
			}
			return ownershipUnknown, containerInspection{}, errors.Join(lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func inspectFakeModelOwnershipReferences(ctx context.Context, cli commandRunner, resource *fakeModelResource, references []string) (ownershipState, containerInspection, error) {
	var errs []error
	for _, reference := range references {
		inspection, exists, err := inspectContainerMaybe(ctx, cli, nil, reference)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect fake model %q: %w", reference, err))
			continue
		}
		if !exists {
			continue
		}
		labels := inspection.Config.Labels
		if inspection.ID == "" || labels["aries.managed"] != "true" || labels["aries.milestone"] != "m5" || labels["aries.kind"] != "fake-model" || labels["aries.attempt"] != resource.AttemptID {
			return ownershipForeign, inspection, errors.New("fake model lacks exact attempt ownership labels")
		}
		return ownershipOwned, inspection, nil
	}
	if len(errs) != 0 {
		return ownershipUnknown, containerInspection{}, errors.Join(errs...)
	}
	return ownershipAbsent, containerInspection{}, nil
}

func applyFakeModelOwnership(resource *fakeModelResource, state ownershipState, inspection containerInspection) {
	switch state {
	case ownershipOwned:
		resource.ID = inspection.ID
		resource.Tentative = false
		resource.Owned = true
	case ownershipAbsent, ownershipForeign:
		resource.ID = ""
		resource.Tentative = false
		resource.Owned = false
	}
}

func removeOwnedFakeModel(ctx context.Context, resource *fakeModelResource) error {
	if resource == nil || (!resource.Tentative && !resource.Owned) {
		return nil
	}
	if resource.Name == "" || resource.AttemptID == "" {
		return errors.New("fake model ownership record is incomplete")
	}
	if resource.Tentative {
		state, inspection, err := awaitFakeModelOwnership(ctx, execRunner{binary: "docker"}, resource, true)
		applyFakeModelOwnership(resource, state, inspection)
		if state == ownershipUnknown {
			return fmt.Errorf("resolve tentative fake model ownership: %w", err)
		}
		if !resource.Owned {
			return nil
		}
	}
	if resource.ID == "" {
		return errors.New("owned fake model has no retained container ID")
	}
	_, exists, err := inspectFakeModelEngine(ctx, *resource)
	if err != nil {
		return err
	}
	if !exists {
		resource.Owned = false
		return nil
	}
	result := integrationDocker(ctx, nil, "container", "rm", "--force", resource.ID)
	if result.exitCode != 0 && !strings.Contains(strings.ToLower(string(result.stderr)), "no such container") {
		return fmt.Errorf("remove owned fake model: %s", result.stderr)
	}
	if err := confirmFakeModelAbsent(ctx, *resource); err != nil {
		return err
	}
	resource.Owned = false
	return nil
}

type fakeEvidence struct {
	Transcript []byte
	Status     struct {
		Terminal string `json:"terminal"`
		Step     int    `json:"step"`
	}
}

type fakeEngineState struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running  bool `json:"Running"`
		PID      int  `json:"Pid"`
		ExitCode int  `json:"ExitCode"`
	} `json:"State"`
}

func inspectFakeModelEngine(ctx context.Context, resource fakeModelResource) (fakeEngineState, bool, error) {
	statusCode, content, err := dockerEngineRequest(ctx, http.MethodGet, "/containers/"+resource.ID+"/json")
	if err != nil {
		return fakeEngineState{}, false, err
	}
	if statusCode == http.StatusNotFound {
		return fakeEngineState{}, false, nil
	}
	if statusCode != http.StatusOK {
		return fakeEngineState{}, false, fmt.Errorf("Docker Engine inspect status %d", statusCode)
	}
	var state fakeEngineState
	if err := json.Unmarshal(content, &state); err != nil {
		return fakeEngineState{}, false, fmt.Errorf("decode Docker Engine fake-model state: %w", err)
	}
	labelsOwned := state.Config.Labels["aries.managed"] == "true" && state.Config.Labels["aries.milestone"] == "m5" && state.Config.Labels["aries.kind"] == "fake-model" && state.Config.Labels["aries.attempt"] == resource.AttemptID
	if state.ID != resource.ID || state.Name != "/"+resource.Name || !labelsOwned {
		return fakeEngineState{}, false, fmt.Errorf("Docker Engine returned unowned fake-model identity %q/%q/%#v, want %q/%q/%q", state.ID, state.Name, state.Config.Labels, resource.ID, "/"+resource.Name, resource.AttemptID)
	}
	return state, true, nil
}

func dockerEngineRequest(ctx context.Context, method, endpoint string) (int, []byte, error) {
	socketPath := "/var/run/docker.sock"
	if dockerHost := os.Getenv("DOCKER_HOST"); dockerHost != "" {
		if !strings.HasPrefix(dockerHost, "unix://") {
			return 0, nil, fmt.Errorf("M5 integration requires a local Unix Docker endpoint, got %q", dockerHost)
		}
		socketPath = strings.TrimPrefix(dockerHost, "unix://")
	}
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return 0, nil, errors.New("Docker Unix socket path is not absolute and clean")
	}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(dialCtx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+endpoint, nil)
	if err != nil {
		return 0, nil, err
	}
	response, err := (&http.Client{Transport: transport}).Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return 0, nil, err
	}
	if len(content) > 1<<20 {
		return 0, nil, errors.New("Docker Engine response exceeded 1 MiB")
	}
	return response.StatusCode, content, nil
}

func stopFakeModelAndReadEvidence(ctx context.Context, evidenceDir string, resource *fakeModelResource) (fakeEvidence, error) {
	proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	if resource == nil || !resource.Owned || resource.ID == "" {
		return fakeEvidence{}, errors.New("fake model is not exactly owned before stop")
	}
	status, err := readStablePrivateFile(filepath.Join(evidenceDir, "status.json"), 0o600)
	if err != nil {
		return fakeEvidence{}, fmt.Errorf("read fake model terminal status before stop: %w", err)
	}
	var evidence fakeEvidence
	if err := json.Unmarshal(status, &evidence.Status); err != nil || evidence.Status.Terminal == "" {
		return fakeEvidence{}, errors.New("fake model terminal status is invalid before stop")
	}
	state, exists, err := inspectFakeModelEngine(proofCtx, *resource)
	if err != nil {
		return fakeEvidence{}, fmt.Errorf("prevalidate fake model before wait: %w", err)
	}
	if !exists {
		return fakeEvidence{}, errors.New("fake model identity is absent before wait")
	}
	if state.State.Running {
		if state.State.PID <= 0 {
			return fakeEvidence{}, fmt.Errorf("prevalidated fake model state is inconsistent: running=%v pid=%d", state.State.Running, state.State.PID)
		}
		stopCtx, stopCancel := context.WithTimeout(proofCtx, 10*time.Second)
		stop := integrationDocker(stopCtx, nil, "container", "stop", "--time", "5", resource.ID)
		stopCancel()
		if stop.exitCode != 0 {
			if err := awaitHostPIDAbsent(proofCtx, state.State.PID); err != nil {
				return fakeEvidence{}, fmt.Errorf("stop owned fake model: %s; PID fallback: %w", stop.stderr, err)
			}
		}
	}
	if _, err := awaitFakeModelStopped(proofCtx, *resource); err != nil {
		return fakeEvidence{}, err
	}
	transcript, err := readStablePrivateFile(filepath.Join(evidenceDir, "transcript.jsonl"), 0o600)
	if err != nil {
		return evidence, fmt.Errorf("read stopped fake model transcript: %w", err)
	}
	evidence.Transcript = transcript
	if err := removeOwnedFakeModel(proofCtx, resource); err != nil {
		return evidence, err
	}
	return evidence, nil
}

func awaitFakeModelStopped(ctx context.Context, resource fakeModelResource) (fakeEngineState, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		state, exists, err := inspectFakeModelEngine(attemptCtx, resource)
		cancel()
		if err == nil && !exists {
			return fakeEngineState{}, errors.New("owned fake model disappeared before stopped-state proof")
		}
		if err == nil && !state.State.Running && state.State.PID == 0 {
			if state.State.ExitCode != 0 {
				return fakeEngineState{}, fmt.Errorf("fake model exited %d", state.State.ExitCode)
			}
			return state, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("state remains running=%v pid=%d exit=%d", state.State.Running, state.State.PID, state.State.ExitCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fakeEngineState{}, fmt.Errorf("prove fake model stopped with exit zero: %w; last=%v", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func awaitHostPIDAbsent(ctx context.Context, pid int) error {
	if pid <= 0 {
		return errors.New("prevalidated fake model PID is not positive")
	}
	path := filepath.Join("/proc", strconv.Itoa(pid))
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("PID %d remains present: %w", pid, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestAwaitHostPIDAbsentIsConservative(t *testing.T) {
	if err := awaitHostPIDAbsent(context.Background(), 1<<30); err != nil {
		t.Fatalf("known-impossible Linux PID was not absent: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := awaitHostPIDAbsent(ctx, os.Getpid()); err == nil {
		t.Fatal("current process PID was reported absent")
	}
}

func confirmFakeModelAbsent(ctx context.Context, resource fakeModelResource) error {
	proofCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lastErr error
	for {
		attemptCtx, attemptCancel := context.WithTimeout(proofCtx, 5*time.Second)
		_, exists, err := inspectFakeModelEngine(attemptCtx, resource)
		attemptCancel()
		if err == nil && !exists {
			return nil
		}
		lastErr = err
		select {
		case <-proofCtx.Done():
			return fmt.Errorf("confirm fake model removal: %w; last engine error=%v", proofCtx.Err(), lastErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func fakeModelScript() string {
	return `const http=require("http"),crypto=require("crypto"),fs=require("fs");
const expectedHash=process.argv[1];
const transcriptPath="/evidence/transcript.jsonl",statusPath="/evidence/status.json",readyPath="/evidence/ready";
let step=0,candidate="",inspectCommand="",mergeCommand="",verifyCommand="",terminalCode=1;
const hold=setInterval(()=>{},1000);
process.on("SIGTERM",()=>{clearInterval(hold);process.exit(terminalCode)});
function record(event){fs.appendFileSync(transcriptPath,JSON.stringify(event)+"\n",{encoding:"utf8",mode:0o600})}
function terminal(kind,code){terminalCode=code;const tmp=statusPath+".tmp";fs.writeFileSync(tmp,JSON.stringify({terminal:kind,step}),{encoding:"utf8",mode:0o600});fs.renameSync(tmp,statusPath);server.close()}
function fail(res,msg){record({event:"rejected",step,reason:msg});res.writeHead(400,{"content-type":"application/json","connection":"close"});res.end(JSON.stringify({error:{message:msg}}),()=>terminal("rejected",1))}
function textOf(content){if(typeof content==="string")return content;if(Array.isArray(content))return content.map(x=>x&&x.text||"").join("\n");return JSON.stringify(content)}
function chain(body,id,command){
  const messages=body.messages||[];
  const assistant=[...messages].reverse().find(m=>m.role==="assistant"&&Array.isArray(m.tool_calls));
  const tool=[...messages].reverse().find(m=>m.role==="tool");
  if(!assistant||!tool||assistant.tool_calls[0].id!==id||tool.tool_call_id!==id||assistant.tool_calls[0].function.name!=="exec")throw new Error("prior tool_call/tool chain mismatch");
  const args=JSON.parse(assistant.tool_calls[0].function.arguments);
  if(args.command!==command)throw new Error("prior exec command mismatch");
  const output=textOf(tool.content);
  record({event:"tool_result",step,id,command,output});
  return output;
}
function sse(res,message,finish,onFlushed){
  const id="aries-chat-"+step;
  record({event:"response",step,finish,has_tool_calls:Array.isArray(message.tool_calls),has_content:typeof message.content==="string"});
  res.writeHead(200,{"content-type":"text/event-stream","cache-control":"no-cache","connection":"close"});
  res.write("data: "+JSON.stringify({id,object:"chat.completion.chunk",created:1,model:"aries-deterministic",choices:[{index:0,delta:message,finish_reason:null}]})+"\n\n");
  res.write("data: "+JSON.stringify({id,object:"chat.completion.chunk",created:1,model:"aries-deterministic",choices:[{index:0,delta:{},finish_reason:finish}]})+"\n\n");
  res.end("data: [DONE]\n\n",onFlushed);
}
function call(res,id,command){record({event:"tool_call",step,id,command,candidate});sse(res,{role:"assistant",tool_calls:[{index:0,id,type:"function",function:{name:"exec",arguments:JSON.stringify({command})}}]},"tool_calls")}
const statusCommand="printf '%s\\n' ARIES_STATUS; git status --short --branch; printf '%s\\n' ARIES_HEAD; git rev-parse HEAD; printf '%s\\n' ARIES_REFLOG; git reflog --all --format='%H %gs' -20";
const server=http.createServer((req,res)=>{
  if(req.method!=="POST"||req.url!=="/v1/chat/completions")return fail(res,"unexpected method or path");
  let body="";
  req.setEncoding("utf8");
  req.on("data",c=>body+=c);
  req.on("end",()=>{try{
    const bearer=(req.headers.authorization||"").replace(/^Bearer /,"");
    if(crypto.createHash("sha256").update(bearer).digest("hex")!==expectedHash)throw new Error("bearer mismatch");
    const parsed=JSON.parse(body);
    if(parsed.model!=="aries-deterministic"||parsed.stream!==true)throw new Error("model or stream mismatch");
    const execTool=(parsed.tools||[]).find(x=>x.type==="function"&&x.function&&x.function.name==="exec");
    if(!execTool||!execTool.function.parameters||!JSON.stringify(execTool.function.parameters).includes("command"))throw new Error("exec schema mismatch");
    record({event:"request",step,model:parsed.model,stream:parsed.stream,message_count:(parsed.messages||[]).length,tool_names:(parsed.tools||[]).map(x=>x.function&&x.function.name||"")});
    if(step===0){call(res,"ariesstatus",statusCommand);step++;return}
    if(step===1){
      const out=chain(parsed,"ariesstatus",statusCommand);
      const head=(out.match(/ARIES_HEAD\s+([0-9a-f]{40})/)||[])[1];
      const hashes=[...out.matchAll(/\b[0-9a-f]{40}\b/g)].map(x=>x[0]);
      candidate=hashes.find(x=>x!==head)||"";
      if(!candidate)throw new Error("candidate hash absent from returned reflog");
      inspectCommand="git show --format=fuller --stat "+candidate+" && git branch --contains "+candidate;
      record({event:"candidate",hash:candidate});
      call(res,"ariesinspect",inspectCommand);step++;return;
    }
    if(step===2){
      const out=chain(parsed,"ariesinspect",inspectCommand);
      if(!out.includes(candidate.slice(0,12))&&!out.includes(candidate))throw new Error("candidate inspection missing hash");
      mergeCommand="git checkout master && git -c user.name='ARIES Benchmark' -c user.email='aries@example.invalid' merge -X theirs --no-edit "+candidate;
      call(res,"ariesmerge",mergeCommand);step++;return;
    }
    if(step===3){
      const out=chain(parsed,"ariesmerge",mergeCommand);
      if(!/fast-forward|merge made|already up.to.date/i.test(out))throw new Error("merge result missing success evidence");
      verifyCommand="git merge-base --is-ancestor "+candidate+" HEAD && test -z \"$(git status --porcelain)\" && git status --short --branch && git log --oneline -5";
      call(res,"ariesverify",verifyCommand);step++;return;
    }
    if(step===4){
      const out=chain(parsed,"ariesverify",verifyCommand);
      if(!out.includes(candidate.slice(0,7))||!out.includes("master"))throw new Error("verification output missing candidate or master");
      record({event:"verified",hash:candidate,output:out});
      step++;
      sse(res,{role:"assistant",content:"Recovered lost commit "+candidate+" and verified a clean master branch."},"stop",()=>terminal("complete",0));return;
    }
    throw new Error("unexpected extra model request");
  }catch(error){fail(res,error.message)}});
});
server.listen(8080,"0.0.0.0",()=>{const tmp=readyPath+".tmp";fs.writeFileSync(tmp,"ready",{encoding:"utf8",mode:0o600});fs.renameSync(tmp,readyPath)});`
}

func requiredHelper(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s must name a built integration helper", name)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

func integrationRepoRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("repository root containing go.mod was not found")
		}
		current = parent
	}
}

func assertPrivateSecretFreeTree(t *testing.T, root, secret string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("artifact directory %q has non-private mode %04o", path, info.Mode().Perm())
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifact path %q is neither a directory nor regular file", path)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("artifact file %q is group/world writable with mode %04o", path, info.Mode().Perm())
		}
		if info.Mode().Perm()&0o044 != 0 && filepath.Base(path) != "aries-exec-helper" {
			return fmt.Errorf("artifact file %q is unexpectedly group/world readable with mode %04o", path, info.Mode().Perm())
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte(secret)) {
			return fmt.Errorf("model credential persisted in %q", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func integrationDocker(ctx context.Context, stdin []byte, args ...string) commandResult {
	result, err := (execRunner{binary: "docker"}).Run(ctx, stdin, args...)
	if err != nil && result.exitCode == 0 {
		result.exitCode = -1
	}
	return result
}

func ensureImage(t *testing.T, ctx context.Context, image string) {
	t.Helper()
	if result := integrationDocker(ctx, nil, "image", "inspect", image); result.exitCode == 0 {
		return
	}
	if result := integrationDocker(ctx, nil, "image", "pull", image); result.exitCode != 0 {
		t.Fatalf("pull pinned image: %s", result.stderr)
	}
}
