//go:build integration

package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	bridgewiring "github.com/hyscale-lab/aries/internal/app/wiring/bridge"
	deploymentwiring "github.com/hyscale-lab/aries/internal/app/wiring/deployment"
	"github.com/hyscale-lab/aries/internal/testutil/dockerroute"
	managedbridge "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/runner"
	tasksandbox "github.com/hyscale-lab/aries/pkg/sandbox"
	"github.com/moby/moby/client"
)

const (
	dshBackgroundMarker = "ARIES_DSH_BACKGROUND_OK"
	dshBackgroundFinal  = "The background service is ready for evaluation."
	dshCanceledMarker   = "ARIES_DSH_CANCELED_OK"
	dshCanceledFinal    = "The background service was canceled and verified stopped."
	dshIntegrationKey   = "aries-dsh-integration-not-a-real-key"
)

const dshBackgroundServer = `/tmp/aries-dsh-runtime/node -e 'require("node:http").createServer((req,res)=>res.end("ARIES_DSH_BACKGROUND_OK")).listen(18765,"127.0.0.1")' >/tmp/aries-background.log 2>&1`
const dshNativeServer = `/tmp/aries-dsh-runtime/node -e 'require("node:fs").writeFileSync("/tmp/aries-background.pid",String(process.pid));require("node:http").createServer((req,res)=>{process.stdout.write("ARIES_DSH_STDOUT\n");process.stderr.write("ARIES_DSH_STDERR\n");res.end("ARIES_DSH_BACKGROUND_OK")}).listen(18765,"127.0.0.1")'`
const dshCanceledServer = `trap '' TERM; ` + `/tmp/aries-dsh-runtime/node -e 'process.on("SIGTERM",()=>{});require("node:fs").writeFileSync("/tmp/aries-background.pid",String(process.pid));require("node:http").createServer((req,res)=>res.end("ARIES_DSH_BACKGROUND_OK")).listen(18765,"127.0.0.1")'`
const dshBackgroundPoll = `/tmp/aries-dsh-runtime/node -e '(async()=>{for(let i=0;i<100;i++){try{if(await(await fetch("http://127.0.0.1:18765")).text()==="ARIES_DSH_BACKGROUND_OK"){console.log("ARIES_DSH_BACKGROUND_OK");return}}catch{};await new Promise(r=>setTimeout(r,20))}process.exit(1)})()'`
const dshServerGoneJS = `const fs=require("node:fs");async function serverGone(){const pid=Number(fs.readFileSync("/tmp/aries-background.pid","utf8"));let alive;try{alive=!["Z","X"].includes(fs.readFileSync("/proc/"+pid+"/stat","utf8").split(") ")[1][0])}catch(e){if(e.code!=="ENOENT")throw e;alive=false}if(alive)return false;try{await fetch("http://127.0.0.1:18765");return false}catch{return true}}`
const dshCanceledPoll = `/tmp/aries-dsh-runtime/node -e '` + dshServerGoneJS + `;(async()=>{for(let i=0;i<250;i++){if(await serverGone()){console.log("ARIES_DSH_CANCELED_OK");return}await new Promise(r=>setTimeout(r,20))}console.error("job_kill left the TERM-ignoring child alive");process.exit(1)})()'`

// The shell exits after the service becomes ready. Its child deliberately stays
// in the shell's process range, as ordinary benchmark background services do.
const dshBackgroundCommand = dshBackgroundServer + " &\n" + dshBackgroundPoll

type dshRunnerHarness struct {
	*Manager
	modelPort, id, endpoint string
}

func (h *dshRunnerHarness) Start(ctx context.Context, request core.HarnessRequest) error {
	route, err := dockerroute.Listen(ctx, request.Connectivity.Placement.AttachmentID)
	if err != nil {
		return err
	}
	request.Model.BaseURL = "http://" + net.JoinHostPort(route.AdvertiseHost, h.modelPort) + "/v1"
	h.endpoint = request.Endpoint.Address
	if err := h.Manager.Start(ctx, request); err != nil {
		return err
	}
	h.id = h.active.ID
	return nil
}

type dshBackgroundBenchmark struct {
	task      core.Task
	api       *client.Client
	harness   *dshRunnerHarness
	live      runner.Sandbox
	sandboxID string
	evaluated bool
	canceled  bool
}

func (b *dshBackgroundBenchmark) Tasks(context.Context) ([]core.Task, error) {
	return []core.Task{b.task}, nil
}

func (b *dshBackgroundBenchmark) PrepareSandbox(_ context.Context, _ core.Task, sandbox runner.Sandbox) error {
	b.live = sandbox
	b.sandboxID = sandbox.(interface{ ContainerID() string }).ContainerID()
	return nil
}

func (b *dshBackgroundBenchmark) Evaluate(ctx context.Context, _ core.Task, sandbox runner.Sandbox, _ runner.EvaluationSandboxes) (core.Evaluation, error) {
	if sandbox != b.live {
		return core.Evaluation{}, errors.New("evaluation received a different sandbox")
	}
	if _, err := b.api.ContainerInspect(ctx, b.harness.id, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		return core.Evaluation{}, fmt.Errorf("harness not positively absent: %v", err)
	}
	host, port, err := net.SplitHostPort(b.harness.endpoint)
	if err != nil {
		return core.Evaluation{}, err
	}
	// Probe from the sandbox, where the bridge's supplied address is reachable.
	// Only after access is closed may the verifier inspect the surviving service.
	verify := `for(let i=0;i<2;i++){const text=await(await fetch("http://127.0.0.1:18765")).text();if(text!=="ARIES_DSH_BACKGROUND_OK")throw Error("unexpected service response");await new Promise(r=>setTimeout(r,50))}console.log("ARIES_DSH_BACKGROUND_OK")`
	want := dshBackgroundMarker
	if b.canceled {
		verify = `if(!(await serverGone()))throw Error("canceled service survived");console.log("ARIES_DSH_CANCELED_OK")`
		want = dshCanceledMarker
	}
	probe := dshServerGoneJS + `;const net=require("node:net");const s=net.createConnection({host:process.argv[1],port:Number(process.argv[2])});s.on("connect",()=>{console.error("bridge listener survived revocation");process.exit(1)});s.on("error",async()=>{try{` + verify + `}catch(e){console.error(e.message);process.exitCode=1}});s.setTimeout(2000,()=>{console.error("bridge revocation probe timed out");process.exit(1)})`
	result, err := sandbox.Exec(ctx, core.Command{Path: "/tmp/aries-dsh-runtime/node", Args: []string{"-e", probe, host, port}, Timeout: 10 * time.Second})
	if err != nil || result.ExitCode != 0 || strings.TrimSpace(result.Stdout) != want {
		return core.Evaluation{}, fmt.Errorf("unexpected background service state after harness stop and bridge revocation (canceled=%t): %#v, %v", b.canceled, result, err)
	}
	b.evaluated = true
	return core.Evaluation{Status: core.StatusSucceeded, Score: 1, Reward: 1}, nil
}

func TestRunnerPreservesDSHBackgroundServiceAndEvidence(t *testing.T) {
	for _, mode := range []string{"shell-child", "shell-child-output", "setsid-child-output", "native-background-job", "promoted-background-job", "explicit-job-kill", "task-cancellation"} {
		t.Run(mode, func(t *testing.T) { runDSHBackgroundScenario(t, mode) })
	}
}

func runDSHBackgroundScenario(t *testing.T, mode string) {
	nativeJob, cancelJob := mode == "native-background-job" || mode == "promoted-background-job" || mode == "explicit-job-kill", mode == "explicit-job-kill"
	cancelTask := mode == "task-cancellation"
	final := dshBackgroundFinal
	if cancelJob {
		final = dshCanceledFinal
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Docker daemon required: %v", err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	versions, err := config.LoadVersions(filepath.Join(root, "configs/versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	image, err := LocalImage(versions.DSH.Image, versions.DSH.Version)
	if err != nil {
		t.Fatal(err)
	}
	bridgeImage, err := BridgeImage(versions.DSH.Image, versions.DSH.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{versions.DSH.Image, image} {
		if _, err := api.ImageInspect(ctx, required); err != nil {
			t.Fatalf("required prepared DSH image %s: %v", required, err)
		}
	}
	if _, err := api.ImageInspect(ctx, bridgeImage); errdefs.IsNotFound(err) {
		recipe, err := os.ReadFile(filepath.Join(root, "Dockerfile.bridge"))
		if err != nil {
			t.Fatal(err)
		}
		if err := dockerdeployment.BuildBridgeImage(ctx, "", bridgeImage, string(recipe), filepath.Join(root, "bin/aries-bridge"), map[string]string{"BASE_IMAGE": image}); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	var toolCalls, toolResults atomic.Int32
	model := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+dshIntegrationKey {
			http.Error(w, "unexpected model request", http.StatusBadRequest)
			return
		}
		var body struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role       string          `json:"role"`
				ToolCallID string          `json:"tool_call_id"`
				Content    json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name       string `json:"name"`
					Parameters struct {
						Properties map[string]json.RawMessage `json:"properties"`
					} `json:"parameters"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ready, canceled, hasBash, hasJobKill, jobStarted, killRequested := false, false, false, false, false, false
		jobID := ""
		for _, message := range body.Messages {
			if message.Role != "tool" {
				continue
			}
			content := string(message.Content)
			ready = ready || strings.Contains(content, dshBackgroundMarker)
			canceled = canceled || message.ToolCallID == "aries-dsh-canceled" && strings.Contains(content, dshCanceledMarker)
			if message.ToolCallID == "aries-dsh-background" {
				jobStarted = true
				if match := regexp.MustCompile(`started background job ([A-Za-z0-9_-]+)`).FindStringSubmatch(content); len(match) == 2 {
					jobID = match[1]
				}
			}
			killRequested = killRequested || message.ToolCallID == "aries-dsh-kill" && strings.Contains(content, "requested cancellation of job "+jobID)
		}
		for _, tool := range body.Tools {
			hasBash = hasBash || tool.Function.Name == "bash"
			_, hasJobID := tool.Function.Parameters.Properties["job_id"]
			hasJobKill = hasJobKill || tool.Function.Name == "job_kill" && hasJobID
		}
		complete := ready && !cancelJob || canceled && cancelJob
		message := map[string]any{"role": "assistant", "content": final}
		finish := "stop"
		if hasBash && !complete {
			toolCalls.Add(1)
			callID, toolName := "aries-dsh-background", "bash"
			arguments := map[string]any{"command": dshBackgroundCommand, "description": "Start background service and confirm its response"}
			if mode == "shell-child-output" {
				arguments["command"] = dshNativeServer + " &\n" + dshBackgroundPoll
			}
			if mode == "setsid-child-output" {
				arguments["command"] = "setsid " + dshNativeServer + " &\n" + dshBackgroundPoll
			}
			if cancelTask {
				// The native tool timeout stays longer than the ARIES task deadline,
				// so this exercises Go cancellation of a still-foreground process.
				arguments["command"], arguments["timeoutMs"] = dshCanceledServer, 60000
			}
			if nativeJob {
				switch {
				case !jobStarted:
					arguments["command"], arguments["run_in_background"] = dshNativeServer, true
					if mode == "promoted-background-job" {
						delete(arguments, "run_in_background")
						arguments["timeoutMs"] = 500
					}
					if cancelJob {
						arguments["command"] = dshCanceledServer
					}
				case !ready:
					callID = "aries-dsh-ready"
					arguments["command"], arguments["description"] = dshBackgroundPoll, "Confirm the native background service responds"
				case cancelJob && !killRequested:
					if !hasJobKill || jobID == "" {
						t.Errorf("native cancellation requires advertised job_kill and a real job id: advertised=%t id=%q", hasJobKill, jobID)
						http.Error(w, "native job cancellation unavailable", http.StatusBadRequest)
						return
					}
					callID, toolName = "aries-dsh-kill", "job_kill"
					arguments = map[string]any{"job_id": jobID, "reason": "Verify explicit cancellation still terminates the process range"}
				case cancelJob:
					callID = "aries-dsh-canceled"
					arguments["command"], arguments["description"] = dshCanceledPoll, "Confirm cancellation removed the TERM ignoring child"
				}
			}
			args, _ := json.Marshal(arguments)
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": toolName, "arguments": string(args)}}}}
			finish = "tool_calls"
		} else if complete {
			toolResults.Add(1)
		}
		if body.Stream {
			if calls, ok := message["tool_calls"].([]any); ok {
				calls[0].(map[string]any)["index"] = 0
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []map[string]any{{"index": 0, "delta": message, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
				data, _ := json.Marshal(map[string]any{"id": "aries-dsh-test", "object": "chat.completion.chunk", "created": 1, "model": "aries-deterministic", "choices": []any{chunk}})
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "aries-dsh-test", "object": "chat.completion", "created": 1, "model": "aries-deterministic", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
	})
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: model, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	output, runID := t.TempDir(), fmt.Sprintf("dsh-background-%d", time.Now().UnixNano())
	runEnvironment := dockerroute.Environment(t, runID)
	provider, err := dockerdeployment.New(dockerdeployment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := tasksandbox.New(tasksandbox.Options{Deployment: provider, NewEnvironment: runEnvironment.NewTaskEnvironment, OutputDir: output})
	if err != nil {
		_ = provider.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sandbox.Close(); err != nil {
			t.Error(err)
		}
	})
	harnessProvider, err := dockerdeployment.New(dockerdeployment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{Deployment: harnessProvider, Image: image, OutputDir: output, AgentTimeout: 90 * time.Second, CleanupTimeout: 45 * time.Second, APIKeyLookup: func(string) ([]byte, bool) { return []byte(dshIntegrationKey), true }}})
	if err != nil {
		_ = harnessProvider.Close()
		t.Fatal(err)
	}
	harness := &dshRunnerHarness{Manager: manager, modelPort: port}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := harness.Stop(cleanup); err != nil {
			t.Error(err)
		}
		if err := harness.Close(); err != nil {
			t.Error(err)
		}
	})
	bridgeProvider, err := dockerdeployment.New(dockerdeployment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	launch := bridgewiring.Launch(bridgeImage)
	launch.RuntimeBackend, launch.ResourceMetrics, launch.Config.Backend = "docker", "docker-stats", "docker"
	launch.Config.BackendEndpoint, launch.Request.Mounts = deploymentwiring.DockerExecutionAccess("")
	service, err := managedbridge.NewService(managedbridge.Options{RunID: runID, Placement: core.RuntimePlacement{AttachmentID: runEnvironment.NetworkName()}, Runtime: bridgeProvider, Launch: launch, BridgeType: "dsh-ssh", RetainRawLog: true, OutputDir: filepath.Join(output, "infrastructure", "bridge")})
	if err != nil {
		_ = bridgeProvider.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := service.Stop(cleanup); err != nil {
			t.Error(err)
		}
	})
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	bridge, err := service.NewSession(output)
	if err != nil {
		t.Fatal(err)
	}
	instruction := "Start the requested background service using bash, verify it responds, and report completion."
	taskTimeout := 90 * time.Second
	if cancelJob {
		instruction = "Start the background service, confirm it responds, cancel its job with job_kill, verify its child stopped, and report completion."
	}
	if cancelTask {
		instruction = "Run the requested server in the foreground and keep it running until interrupted."
		taskTimeout = 7 * time.Second
	}
	benchmark := &dshBackgroundBenchmark{api: api, harness: harness, canceled: cancelJob || cancelTask, task: core.Task{ID: "background-service", Instruction: instruction, Timeout: taskTimeout, Environment: core.Environment{Image: versions.DSH.Image, Workdir: "/app", AllowNetwork: true}}}
	run, err := runner.New(benchmark, harness, sandbox, bridge, runner.Options{RunID: runID, OutputDir: output, CleanupTimeout: time.Minute, Model: core.ModelConfig{Provider: "openai", BaseURL: "http://127.0.0.1/v1", Model: "aries-deterministic", APIKeyEnv: "ARIES_DSH_INTEGRATION_KEY"}})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := run.Run(ctx)
	if len(result.Tasks) != 1 || !cancelTask && runErr != nil || cancelTask && runErr == nil {
		stderr, _ := os.ReadFile(filepath.Join(output, benchmark.task.ID, "harness", "stderr.log"))
		t.Fatalf("real DSH Runner: %v; %#v; stderr: %s", runErr, result, stderr)
	}
	task := result.Tasks[0]
	wantCalls := int32(1)
	if nativeJob {
		wantCalls = 2
	}
	if cancelJob {
		wantCalls = 4
	}
	wantStatus, wantFinal, modelResultOK := core.StatusSucceeded, final, toolResults.Load() > 0
	if cancelTask {
		wantStatus, wantFinal, modelResultOK = core.StatusCanceled, "", toolResults.Load() == 0
	}
	if task.Harness.Status != wantStatus || task.Harness.FinalResponse != wantFinal || task.Evaluation.Score != 1 || task.Evaluation.Reward != 1 || !benchmark.evaluated || task.Isolation.Status != core.StatusConfirmed || !task.Isolation.HarnessStopped || !task.Isolation.BridgeRevoked || task.Cleanup.Status != core.StatusSucceeded || toolCalls.Load() != wantCalls || !modelResultOK {
		t.Fatalf("incomplete Runner result: %#v; tool calls/results %d/%d", task, toolCalls.Load(), toolResults.Load())
	}
	for _, id := range []string{harness.id, benchmark.sandboxID} {
		if _, err := api.ContainerInspect(ctx, id, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Errorf("container %s not positively absent: %v", id, err)
		}
	}
	dshAssertEvidence(t, output, benchmark.task.ID, task, wantFinal)
	bridgeID, networkName := service.RuntimeID(), runEnvironment.NetworkName()
	if err := service.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := api.ContainerInspect(ctx, bridgeID, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Errorf("bridge runtime not positively absent: %v", err)
	}
	if err := runEnvironment.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NetworkInspect(ctx, networkName, client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Errorf("run network not positively absent: %v", err)
	}
}

func dshAssertEvidence(t *testing.T, output, taskID string, task core.TaskResult, final string) {
	t.Helper()
	root := filepath.Join(output, taskID, "harness")
	for _, name := range []string{"config.json", "instruction.txt", "events.jsonl", "stderr.log", "outcome.json"} {
		path := filepath.Join(root, name)
		found := false
		for _, retained := range task.Harness.LogPaths {
			found = found || retained == path
		}
		if !found {
			t.Errorf("harness result does not reference %s", name)
		}
		if _, err := os.Stat(path); err != nil {
			t.Error(err)
		}
	}
	events, err := os.ReadFile(filepath.Join(root, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	observed, parseErr := parseOutcome(events)
	if final == "" {
		if parseErr == nil {
			t.Fatalf("canceled run fabricated a completed final turn: %s", events)
		}
	} else if parseErr != nil || observed != final || !strings.Contains(string(events), `"type":"tool_result"`) {
		t.Fatalf("incomplete native headless events: %v; %s", parseErr, events)
	}
	if !strings.Contains(string(events), `"type":"tool_call"`) {
		t.Fatalf("native headless events lost the interrupted or completed tool call: %s", events)
	}
	outcomeBytes, err := os.ReadFile(filepath.Join(root, "outcome.json"))
	if err != nil {
		t.Fatal(err)
	}
	var timing struct {
		StartedAt  time.Time `json:"started_at"`
		FinishedAt time.Time `json:"finished_at"`
		ExitCode   int       `json:"exit_code"`
	}
	if err := json.Unmarshal(outcomeBytes, &timing); err != nil || timing.StartedAt.IsZero() || timing.FinishedAt.Before(timing.StartedAt) || (timing.ExitCode == 0) != (final != "") {
		t.Fatalf("incomplete native outcome: %v; %s", err, outcomeBytes)
	}
	var sessions strings.Builder
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), dshIntegrationKey) {
			t.Errorf("credential leaked into %s", path)
		}
		if strings.HasPrefix(path, filepath.Join(root, "sessions")+string(filepath.Separator)) && strings.HasSuffix(path, ".jsonl") {
			sessions.Write(content)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	markers := []string{"createServer", dshBackgroundMarker, `"type":"request/context"`, `"type":"tool/call"`, `"type":"assistant/message"`}
	if final != "" {
		markers = append(markers, final, `"type":"tool/result"`)
	}
	for _, marker := range markers {
		if !strings.Contains(sessions.String(), marker) {
			t.Errorf("native sessions lost command/result/final marker %q", marker)
		}
	}
	for _, name := range []string{"tool-calls.jsonl", "ssh_raw.log"} {
		path := filepath.Join(output, taskID, "bridge", name)
		content, err := os.ReadFile(path)
		if err != nil || len(content) == 0 {
			t.Errorf("missing bridge evidence %s: %v", name, err)
		}
		found := false
		for _, retained := range task.ToolLogPaths {
			found = found || retained == path
		}
		if !found {
			t.Errorf("task result does not reference %s", name)
		}
	}
}
