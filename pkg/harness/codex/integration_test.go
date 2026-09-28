//go:build integration

package codex_test

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/bridge/codexssh"
	"github.com/hyscale-lab/aries/pkg/core"
	codexharness "github.com/hyscale-lab/aries/pkg/harness/codex"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const integrationImage = "docker.io/library/debian:12.12-slim"
const integrationKey = "sk-codex-integration-not-a-real-model-key"
const integrationQwen = "Qwen/Qwen3.8-27B"
const integrationInstructions = "ARIES research policy: delegate independent work and omit child model, reasoning_effort, and agent_type overrides to inherit the parent settings."

// This is the complete production path: upstream Codex emits a native tool
// call, its native exec-server receives it over the SSH bridge, and evaluation
// sees that exact task container only after both isolation gates succeed.
func TestCodexNativeSSHMutatesEvaluatorSandbox(t *testing.T) {
	runNativeSSHScenario(t, false, "", false)
}

func TestCodexNativeSSHCancelReapsEscapedChildrenOnly(t *testing.T) {
	runNativeSSHScenario(t, true, "", false)
}

func TestCodexNativeSSHRetainsNonrootTaskIdentity(t *testing.T) {
	runNativeSSHScenario(t, false, "65532:65532", false)
}

func TestCodexNativeSubagentInheritsQwenReasoningAndSSHExecutor(t *testing.T) {
	runNativeSSHScenario(t, false, "", true)
}

func runNativeSSHScenario(t *testing.T, cancelTool bool, taskUser string, subagents bool) {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := integrationExecutable(t, "ARIES_CODEX_BINARY", filepath.Join(root, ".cache", "codex", "0.157.1", "codex"))
	helper := integrationExecutable(t, "ARIES_CODEX_SSH_CLIENT", filepath.Join(root, "bin", "aries-codex-ssh"))
	supervisor := integrationExecutable(t, "ARIES_CODEX_EXEC_SUPERVISOR", filepath.Join(root, "bin", "aries-codex-exec"))
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	if _, err := api.ImageInspect(ctx, integrationImage); err != nil {
		if !errdefs.IsNotFound(err) {
			t.Fatal(err)
		}
		pull, err := api.ImagePull(ctx, integrationImage, client.ImagePullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer pull.Close()
		if err := pull.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	output := t.TempDir()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	runID := fmt.Sprintf("codex-integration-%d", time.Now().UnixNano())
	// This is registered before component cleanup, so it runs after the
	// containers/network are removed and before the Docker client is closed,
	// including when an earlier assertion aborts the test.
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		filters := client.Filters{}.Add("label", "aries.run="+runID)
		remaining, err := api.ContainerList(cleanup, client.ContainerListOptions{All: true, Filters: filters})
		if err != nil || len(remaining.Items) != 0 {
			t.Errorf("owned container leak count=%d err=%v", len(remaining.Items), err)
		}
		networks, err := api.NetworkList(cleanup, client.NetworkListOptions{Filters: filters})
		if err != nil || len(networks.Items) != 0 {
			t.Errorf("owned network leak count=%d err=%v", len(networks.Items), err)
		}
	})
	sandboxes, err := dockersandbox.New(dockersandbox.Options{OutputDir: output, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sandboxes.Close() })
	live, err := sandboxes.Start(ctx, core.SandboxRequest{RunID: runID, TaskID: "native-ssh", Environment: core.Environment{Image: integrationImage, Workdir: "/app", MemoryMB: 1024, AllowNetwork: true, ExecUser: taskUser}})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*dockersandbox.Sandbox)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := sandboxes.Stop(cleanup, live); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
	})
	if taskUser != "" {
		prepared, err := sandbox.Exec(ctx, core.Command{Path: "/bin/chmod", Args: []string{"0777", "/app"}, User: "0:0"})
		if err != nil || prepared.ExitCode != 0 {
			t.Fatalf("prepare nonroot task workspace: %#v, %v", prepared, err)
		}
	}
	if cancelTool {
		// This task-owned process predates the executor's subreaper and must
		// survive bridge revocation even though native descendants do not.
		other, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", `sleep 600 </dev/null >/dev/null 2>&1 & echo $! > /app/existing.pid`}})
		if err != nil || other.ExitCode != 0 {
			t.Fatalf("start preexisting task process: %#v, %v", other, err)
		}
	}
	bridge, err := codexssh.New(codexssh.Options{ClientPath: helper, CodexPath: binary, SupervisorPath: supervisor, OutputDir: output, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := bridge.Stop(cleanup); err != nil {
			t.Errorf("bridge cleanup: %v", err)
		}
	})
	endpoint, err := bridge.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := sandbox.NetworkGateway(ctx)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		t.Fatal(err)
	}
	model := &responsesFixture{cancelTool: cancelTool, taskUser: taskUser, subagents: subagents}
	server := &http.Server{Handler: model, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	options := codexharness.Options{Image: integrationImage, CodexPath: binary, CodexVersion: "0.157.1", OutputDir: output, Logger: logger, StartTimeout: 90 * time.Second, AgentTimeout: 90 * time.Second, APIKeyLookup: func(string) ([]byte, bool) { return []byte(integrationKey), true }}
	if subagents {
		options.ReasoningEffort = "xhigh"
		options.DeveloperInstructions = integrationInstructions
		options.SubagentsEnabled = true
		options.MaxConcurrentSubagents = 2
	}
	harness, err := codexharness.New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := harness.Stop(cleanup); err != nil {
			t.Errorf("harness cleanup: %v", err)
		}
		_ = harness.Close()
	})
	request := core.HarnessRequest{RunID: runID, TaskID: "native-ssh", Endpoint: endpoint, Model: core.ModelConfig{Provider: "openai", BaseURL: "http://" + listener.Addr().String() + "/v1", Model: "aries-deterministic-codex", APIKeyEnv: "ARIES_CODEX_TEST_KEY"}}
	if subagents {
		request.Model.Model = integrationQwen
	}
	if err := harness.Start(ctx, request); err != nil {
		t.Fatal(err)
	}
	filters := client.Filters{}.Add("label", "aries.run="+runID).Add("label", "aries.kind=codex-harness")
	containers, err := api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil || len(containers.Items) != 1 {
		t.Fatalf("harness container inventory: %v, count=%d", err, len(containers.Items))
	}
	harnessID := containers.Items[0].ID
	if harnessID == sandbox.ContainerID() {
		t.Fatal("harness and task share a container")
	}
	inspection, err := api.ContainerInspect(ctx, harnessID, client.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(inspection)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(metadata, []byte(integrationKey)) {
		t.Fatal("model credential entered Docker metadata")
	}
	var result core.HarnessResult
	var runErr error
	if cancelTool {
		result, runErr = cancelNativeTool(t, ctx, harness, sandbox)
	} else {
		result, runErr = harness.Run(ctx, "Use the terminal to complete the task in /app, then report completion.")
	}
	if err := harness.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	validResult := runErr == nil && result.Status == core.StatusSucceeded && result.FinalResponse == "native Codex SSH complete"
	if cancelTool {
		validResult = errors.Is(runErr, context.Canceled) && result.Status == core.StatusCanceled
	}
	if !validResult {
		for _, filename := range result.LogPaths {
			if strings.HasSuffix(filename, "stderr.log") || strings.HasSuffix(filename, "trajectory.jsonl") {
				content, _ := os.ReadFile(filename)
				t.Logf("%s:\n%s", filepath.Base(filename), content)
			}
		}
		t.Fatalf("native Codex run: status=%s final=%q error=%v", result.Status, result.FinalResponse, runErr)
	}
	model.mu.Lock()
	calls, modelErr := model.calls, model.err
	model.mu.Unlock()
	wantCalls := 2
	if cancelTool {
		wantCalls = 1
	} else if subagents {
		wantCalls = 6 // Four parent requests and two requests from its real child.
	}
	if calls != wantCalls || modelErr != nil {
		t.Fatalf("Responses exchange calls=%d error=%v", calls, modelErr)
	}
	if cancelTool {
		checked, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", `set -eu; test ! -d "/proc/$(cat /app/native.pid)"; test ! -d "/proc/$(cat /app/escaped.pid)"; kill -0 "$(cat /app/existing.pid)"`}})
		if err != nil || checked.ExitCode != 0 {
			t.Fatalf("native descendants survived or unrelated task process was killed: %#v, %v", checked, err)
		}
	}
	// Cleanup must use the supervisor's trusted Go code before its proof. A
	// task-controlled rm must never run after native descendants are reaped.
	// Use a raw SDK exec here: the ordinary sandbox.Exec wrapper itself calls
	// rm, which would contaminate this test's independent verification.
	verification := `set -eu; test ! -e /app/cleanup-compromised; for entry in /.aries-codex-*; do test ! -e "$entry" && test ! -L "$entry"; done`
	if !cancelTool {
		verification += `; test "$(cat /app/native-state)" = native-exec-state`
	}
	if !cancelTool && taskUser == "" && !subagents {
		verification += `; case "$(cat /bin/rm)" in */app/cleanup-compromised*) ;; *) exit 1;; esac`
	}
	if subagents {
		verification += `; test "$(cat /app/parent-state)" = native-parent-state`
	}
	created, err := api.ExecCreate(ctx, sandbox.ContainerID(), client.ExecCreateOptions{Cmd: []string{"/bin/sh", "-c", verification}, User: "0:0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.ExecStart(ctx, created.ID, client.ExecStartOptions{Detach: true}); err != nil {
		t.Fatal(err)
	}
	verificationCtx, verificationCancel := context.WithTimeout(ctx, 10*time.Second)
	defer verificationCancel()
	for {
		checked, err := api.ExecInspect(verificationCtx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !checked.Running {
			if checked.ExitCode != 0 {
				t.Fatal("cleanup invoked task rm, retained executor staging, or changed native task state")
			}
			break
		}
		select {
		case <-verificationCtx.Done():
			t.Fatal(verificationCtx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if _, err := api.ContainerInspect(ctx, harnessID, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("harness absence was not confirmed: %v", err)
	}
	if connection, err := net.DialTimeout("tcp", endpoint.Address, 200*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("revoked bridge still accepts connections")
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bridge identity retained after revoke: %v", err)
	}
	for _, filename := range append(result.LogPaths, endpoint.LogPaths...) {
		content, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(content, []byte(integrationKey)) {
			t.Fatalf("credential in artifact %s", filename)
		}
		info, err := os.Stat(filename)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact is not private: %s", filename)
		}
	}
	trajectory, err := os.ReadFile(filepath.Join(output, "native-ssh", "harness", "trajectory.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if cancelTool {
		// Native tool execution is established by the ready/PID gates above.
		// Cancellation can precede its JSONL item and truncate the final record.
		threadStarted, turnStarted := false, false
		for _, line := range bytes.Split(trajectory, []byte{'\n'}) {
			var event struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(line, &event) != nil {
				continue
			}
			threadStarted = threadStarted || event.Type == "thread.started"
			turnStarted = turnStarted || event.Type == "turn.started"
		}
		if !threadStarted || !turnStarted {
			t.Fatalf("canceled native trajectory has no valid thread/turn start: %s", trajectory)
		}
	} else if !bytes.Contains(trajectory, []byte(`"type":"command_execution"`)) || !bytes.Contains(trajectory, []byte(`"type":"turn.completed"`)) {
		t.Fatalf("native trajectory missing expected tool/turn evidence: %s", trajectory)
	}
	if subagents {
		if !bytes.Contains(trajectory, []byte(`"tool":"spawn_agent"`)) || !bytes.Contains(trajectory, []byte(`"tool":"wait"`)) {
			t.Fatalf("native trajectory has no completed delegation: %s", trajectory)
		}
		// Both parent and child execute through one native exec-server. The
		// bridge would reject another SSH executor, and its audit must agree.
		audit, err := os.ReadFile(filepath.Join(output, "native-ssh", "bridge", "tool-calls.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(audit), []byte{'\n'})
		var record struct {
			Container string `json:"container_id"`
			Operation string `json:"operation_class"`
			Status    string `json:"status"`
			ExitCode  int    `json:"exit_code"`
		}
		if len(lines) != 1 || json.Unmarshal(lines[0], &record) != nil || record.Container != sandbox.ContainerID() || record.Operation != "executor" || record.Status != "completed" || record.ExitCode != 0 {
			t.Fatalf("parent/child did not share one clean SSH executor: %s", audit)
		}
	}
	if err := sandboxes.Stop(ctx, live); err != nil {
		t.Fatal(err)
	}
}

func cancelNativeTool(t *testing.T, ctx context.Context, harness *codexharness.Manager, sandbox *dockersandbox.Sandbox) (core.HarnessResult, error) {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		result core.HarnessResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		result, err := harness.Run(runCtx, "Use the terminal to work in /app.")
		finished <- outcome{result, err}
	}()
	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	defer readyCancel()
	for {
		ready, err := sandbox.Exec(readyCtx, core.Command{Path: "/bin/sh", Args: []string{"-c", `test -s /app/native.started && test -s /app/native.pid && test -s /app/escaped.pid`}})
		if err == nil && ready.ExitCode == 0 {
			break
		}
		select {
		case ended := <-finished:
			t.Errorf("native tool ended before the cancellation gate: %v", ended.err)
			return ended.result, ended.err
		case <-readyCtx.Done():
			t.Errorf("native command and escaped child did not reach the cancellation gate: %v", readyCtx.Err())
			cancel()
			ended := <-finished
			return ended.result, ended.err
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	select {
	case ended := <-finished:
		return ended.result, ended.err
	case <-ctx.Done():
		t.Fatalf("Codex cancellation did not finish: %v", ctx.Err())
		return core.HarnessResult{}, ctx.Err()
	}
}

func integrationExecutable(t *testing.T, variable, fallback string) string {
	t.Helper()
	filename := os.Getenv(variable)
	if filename == "" {
		filename = fallback
	}
	info, err := os.Stat(filename)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("native Codex integration requires %s or %s", variable, fallback)
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("invalid integration executable %s: %v", filename, err)
	}
	return filename
}

type responsesFixture struct {
	mu                    sync.Mutex
	calls                 int
	err                   error
	cancelTool            bool
	taskUser              string
	subagents             bool
	parentCalls           int
	childCalls            int
	command               string
	privateMarker         string
	developerInstructions string
}

func (fixture *responsesFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if request.Method != http.MethodPost || request.URL.Path != "/v1/responses" {
		http.NotFound(writer, request)
		return
	}
	if request.Header.Get("Authorization") != "Bearer "+integrationKey {
		fixture.err = errors.New("Responses credential header is missing")
		http.Error(writer, "missing credentials", http.StatusUnauthorized)
		return
	}
	content, err := io.ReadAll(io.LimitReader(request.Body, 4<<20))
	if err != nil {
		fixture.err = err
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	if bytes.Contains(content, []byte(integrationKey)) {
		fixture.err = errors.New("credential entered Responses JSON")
		http.Error(writer, "invalid request", http.StatusBadRequest)
		return
	}
	if fixture.privateMarker != "" && bytes.Contains(content, []byte(fixture.privateMarker)) {
		fixture.err = errors.New("private verifier entered Responses JSON")
		http.Error(writer, "private verifier input", http.StatusBadRequest)
		return
	}
	fixture.calls++
	var item map[string]any
	if fixture.subagents {
		item, err = fixture.subagentResponse(request, content)
		if err != nil {
			fixture.err = err
			http.Error(writer, "invalid subagent exchange", http.StatusBadRequest)
			return
		}
	} else {
		switch fixture.calls {
		case 1:
			// The key name is deliberately visible; its value must be absent in
			// the native tool environment and its private harness file unreachable.
			command := `set -eu; test -z "${ARIES_CODEX_TEST_KEY+x}"; test ! -e /run/aries/codex/model.key; test "$PWD" = /app; `
			if fixture.taskUser == "" {
				command += `printf '#!/bin/sh\nprintf compromised > /app/cleanup-compromised\nexit 0\n' > /bin/rm; chmod 0755 /bin/rm; `
			} else {
				command += `test "$(id -u):$(id -g)" = 65532:65532; `
			}
			command += `printf native-exec-state > /app/native-state; printf REMOTE_CODEX_OK`
			if fixture.cancelTool {
				command = `set -eu; test -z "${ARIES_CODEX_TEST_KEY+x}"; echo $$ > /app/native.pid; setsid /bin/sh -c 'echo $$ > /app/escaped.pid; exec sleep 600' </dev/null >/dev/null 2>&1 & while [ ! -s /app/escaped.pid ]; do sleep 0.02; done; printf ready > /app/native.started; sleep 600`
			}
			if fixture.command != "" {
				command = fixture.command
			}
			arguments, _ := json.Marshal(map[string]any{"cmd": command, "workdir": "/app", "login": false, "yield_time_ms": 10000, "max_output_tokens": 1000})
			item = map[string]any{"type": "function_call", "call_id": "call-native-exec", "name": "exec_command", "arguments": string(arguments)}
		case 2:
			var body struct {
				Input []struct {
					Type   string          `json:"type"`
					Output json.RawMessage `json:"output"`
				} `json:"input"`
			}
			if err := json.Unmarshal(content, &body); err != nil {
				fixture.err = err
			}
			toolOutput := false
			for _, input := range body.Input {
				if input.Type == "function_call_output" && bytes.Contains(input.Output, []byte("REMOTE_CODEX_OK")) {
					toolOutput = true
				}
			}
			if !toolOutput {
				fixture.err = errors.New("native remote command did not return its marker")
			}
			item = map[string]any{"type": "message", "role": "assistant", "id": "message-final", "content": []any{map[string]any{"type": "output_text", "text": "native Codex SSH complete"}}}
		default:
			fixture.err = errors.New("unexpected additional Responses request")
			http.Error(writer, "too many requests", http.StatusBadRequest)
			return
		}
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	for _, event := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("response-%d", fixture.calls)}},
		{"type": "response.output_item.done", "item": item},
		{"type": "response.completed", "response": map[string]any{"id": fmt.Sprintf("response-%d", fixture.calls), "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}},
	} {
		encoded, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event["type"], encoded)
	}
}

type subagentResponseRequest struct {
	Model     string `json:"model"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Input []struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	} `json:"input"`
}

func (fixture *responsesFixture) subagentResponse(request *http.Request, content []byte) (map[string]any, error) {
	var body subagentResponseRequest
	if err := json.Unmarshal(content, &body); err != nil {
		return nil, err
	}
	instructions := fixture.developerInstructions
	if instructions == "" {
		instructions = integrationInstructions
	}
	if body.Model != integrationQwen || body.Reasoning.Effort != "xhigh" || !bytes.Contains(content, []byte(instructions)) {
		return nil, fmt.Errorf("native model/effort/developer inheritance mismatch: model=%q effort=%q", body.Model, body.Reasoning.Effort)
	}
	if request.Header.Get("X-OpenAI-Subagent") == "collab_spawn" {
		fixture.childCalls++
		switch fixture.childCalls {
		case 1:
			if request.Header.Get("X-Codex-Parent-Thread-Id") == "" {
				return nil, errors.New("native child request has no parent thread identity")
			}
			command := `set -eu; test -z "${ARIES_CODEX_TEST_KEY+x}"; test ! -e /run/aries/codex/model.key; test "$PWD" = /app; printf native-exec-state > /app/native-state; `
			if fixture.command != "" {
				command += fixture.command + "; "
			}
			command += "printf REMOTE_CHILD_OK"
			return nativeFunctionCall("call-child-exec", "", "exec_command", map[string]any{"cmd": command, "workdir": "/app", "login": false, "yield_time_ms": 10000}), nil
		case 2:
			if !strings.Contains(body.functionOutput("call-child-exec"), "REMOTE_CHILD_OK") {
				return nil, errors.New("native child did not execute in the task sandbox")
			}
			return nativeMessage("native child complete"), nil
		default:
			return nil, errors.New("unexpected extra native child request")
		}
	}
	if request.Header.Get("X-OpenAI-Subagent") != "" {
		return nil, errors.New("unexpected subagent identity")
	}
	fixture.parentCalls++
	switch fixture.parentCalls {
	case 1:
		if !bytes.Contains(content, []byte(`"spawn_agent"`)) {
			return nil, errors.New("native delegation tool was not exposed")
		}
		return nativeFunctionCall("call-spawn", "multi_agent_v1", "spawn_agent", map[string]any{"message": "Use the terminal to write the native child marker in /app, then report completion."}), nil
	case 2:
		var spawned struct {
			AgentID string `json:"agent_id"`
		}
		if json.Unmarshal([]byte(body.functionOutput("call-spawn")), &spawned) != nil || spawned.AgentID == "" {
			return nil, errors.New("native spawn_agent did not return a child id")
		}
		return nativeFunctionCall("call-wait", "multi_agent_v1", "wait_agent", map[string]any{"targets": []string{spawned.AgentID}, "timeout_ms": 60000}), nil
	case 3:
		if !strings.Contains(body.functionOutput("call-wait"), "native child complete") {
			return nil, errors.New("native wait_agent did not observe child completion")
		}
		return nativeFunctionCall("call-parent-exec", "", "exec_command", map[string]any{"cmd": `set -eu; test "$(cat /app/native-state)" = native-exec-state; test -z "${ARIES_CODEX_TEST_KEY+x}"; printf native-parent-state > /app/parent-state; printf REMOTE_PARENT_OK`, "workdir": "/app", "login": false, "yield_time_ms": 10000}), nil
	case 4:
		if !strings.Contains(body.functionOutput("call-parent-exec"), "REMOTE_PARENT_OK") {
			return nil, errors.New("native parent did not observe the child's sandbox state")
		}
		return nativeMessage("native Codex SSH complete"), nil
	default:
		return nil, errors.New("unexpected extra native parent request")
	}
}

func (body subagentResponseRequest) functionOutput(callID string) string {
	for _, input := range body.Input {
		if input.Type != "function_call_output" || input.CallID != callID {
			continue
		}
		var text string
		if json.Unmarshal(input.Output, &text) == nil {
			return text
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(input.Output, &parts) == nil {
			for _, part := range parts {
				text += part.Text
			}
			return text
		}
	}
	return ""
}

func nativeFunctionCall(callID, namespace, name string, args map[string]any) map[string]any {
	encoded, _ := json.Marshal(args)
	item := map[string]any{"type": "function_call", "call_id": callID, "name": name, "arguments": string(encoded)}
	if namespace != "" {
		item["namespace"] = namespace
	}
	return item
}

func nativeMessage(text string) map[string]any {
	return map[string]any{"type": "message", "role": "assistant", "id": "message-final", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}
