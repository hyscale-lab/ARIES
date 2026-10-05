//go:build integration

package hermes

import (
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/hyscale-lab/aries/pkg/runner"
	tasksandbox "github.com/hyscale-lab/aries/pkg/sandbox"
	"github.com/moby/moby/client"
)

// runnerHermes records the real runtime identity and points the model at the
// deterministic host endpoint reachable through the task's Docker gateway.
type runnerHermes struct {
	*Manager
	modelPort    string
	id           string
	endpoint     string
	identityPath string
}

func (h *runnerHermes) Start(ctx context.Context, request core.HarnessRequest) error {
	host, _, err := net.SplitHostPort(request.Endpoint.Address)
	if err != nil {
		return err
	}
	request.Model.BaseURL = "http://" + net.JoinHostPort(host, h.modelPort) + "/v1"
	h.endpoint = request.Endpoint.Address
	h.identityPath = request.Endpoint.IdentitySourceFile
	if err := h.Manager.Start(ctx, request); err != nil {
		return err
	}
	h.id = h.active.ID
	return nil
}

type hermesRunnerBenchmark struct {
	task                           core.Task
	api                            *client.Client
	harness                        *runnerHermes
	live                           runner.Sandbox
	containerID, network, verifier string
	evaluated                      bool
}

func (b *hermesRunnerBenchmark) Tasks(context.Context) ([]core.Task, error) {
	return []core.Task{b.task}, nil
}
func (b *hermesRunnerBenchmark) PrepareSandbox(ctx context.Context, _ core.Task, s runner.Sandbox) error {
	b.live = s
	identity := s.(interface {
		ContainerID() string
		Connectivity() core.HarnessConnectivity
	})
	b.containerID, b.network = identity.ContainerID(), identity.Connectivity().Placement.DockerNetwork
	result, err := s.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "test ! -e /tmp/aries-private-verifier && test ! -e /tmp/aries-hermes-proof"}})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return errors.New("task fixture is not clean")
	}
	return nil
}
func (b *hermesRunnerBenchmark) Evaluate(ctx context.Context, _ core.Task, s runner.Sandbox) (core.Evaluation, error) {
	if s != b.live {
		return core.Evaluation{}, errors.New("evaluation received a different sandbox")
	}
	if _, err := b.api.ContainerInspect(ctx, b.harness.id, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		return core.Evaluation{}, fmt.Errorf("harness not positively absent: %v", err)
	}
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", b.harness.endpoint)
	if err == nil {
		_ = conn.Close()
		return core.Evaluation{}, errors.New("bridge listener survived revocation")
	}
	// Verifier material remains host-private until both real isolation gates hold.
	if err := s.Upload(ctx, b.verifier, "/tmp/aries-private-verifier"); err != nil {
		return core.Evaluation{}, err
	}
	result, err := s.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"/tmp/aries-private-verifier"}})
	if err != nil || result.ExitCode != 0 {
		return core.Evaluation{}, fmt.Errorf("evaluate bridge mutation: %#v, %v", result, err)
	}
	b.evaluated = true
	return core.Evaluation{Status: core.StatusSucceeded, Score: 1, Reward: 1}, nil
}

func TestRunnerThroughRealHermesSSHBridge(t *testing.T) {
	runHermesBridgeScenario(t, false, 1, true)
}

func TestGatewayRepeatedAndConcurrentTaskOccurrences(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) { t.Parallel(); runHermesBridgeScenario(t, false, 2, false) })
	}
}

func TestGatewayCancelsDuringSandboxCommand(t *testing.T) {
	runHermesBridgeScenario(t, true, 1, false)
}

func runHermesBridgeScenario(t *testing.T, cancelCommand bool, repetitions int, reasoning bool, derivedImage ...string) {
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
	fixture, err := terminalbench.New(terminalbench.Options{Root: filepath.Join(root, terminalbench.DefaultRoot), TaskIDs: []string{"fix-git"}, Revision: versions.TerminalBench2.Revision, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := fixture.Tasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("load sandbox fixture: %v", err)
	}
	for _, image := range []string{versions.Hermes.Image, tasks[0].Environment.Image} {
		if _, err := api.ImageInspect(ctx, image); err != nil {
			t.Fatalf("required prepared image %s: %v", image, err)
		}
	}
	const key = "aries-hermes-runner-fake-key"
	var toolResponses atomic.Int32
	var reasoningReplays atomic.Int32
	const reasoningMarker = "aries-reasoning-fixture"
	model := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "unexpected route or credentials", 400)
			return
		}
		var body struct {
			Stream          bool   `json:"stream"`
			ReasoningEffort string `json:"reasoning_effort"`
			Thinking        struct {
				Type string `json:"type"`
			} `json:"thinking"`
			Messages []struct {
				Role             string          `json:"role"`
				ReasoningContent string          `json:"reasoning_content"`
				Content          json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		terminal := false
		for _, tool := range body.Tools {
			terminal = terminal || tool.Function.Name == "terminal"
		}
		message := map[string]any{"role": "assistant", "content": "Completed the sandbox mutation."}
		finish := "stop"
		if terminal {
			if reasoning && (body.Thinking.Type != "enabled" || body.ReasoningEffort != "high") {
				t.Errorf("missing explicit DeepSeek reasoning controls: thinking=%s effort=%s", body.Thinking.Type, body.ReasoningEffort)
				http.Error(w, "missing reasoning controls", 400)
				return
			}
			observed := false
			replayed := false
			for _, m := range body.Messages {
				if m.Role == "assistant" && m.ReasoningContent == reasoningMarker {
					replayed = true
				}
				if m.Role == "tool" && strings.Contains(string(m.Content), "ARIES_HERMES_BRIDGE_OK") {
					observed = true
				}
			}
			if reasoning && observed {
				if !replayed {
					t.Error("DeepSeek tool continuation lost reasoning_content")
					http.Error(w, "missing reasoning replay", 400)
					return
				}
				reasoningReplays.Add(1)
			}
			if !observed {
				toolResponses.Add(1)
				command := "test ! -e /tmp/aries-private-verifier && test ! -e " + modelKeyPath + " && test \"$(pwd)\" = " + tasks[0].Environment.Workdir + " && printf 'ARIES_HERMES_BRIDGE_OK\\n' > /tmp/aries-hermes-proof && cat /tmp/aries-hermes-proof"
				if cancelCommand {
					command += "; sleep 600"
				}
				args, _ := json.Marshal(map[string]any{"command": command})
				message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "aries-terminal", "type": "function", "function": map[string]any{"name": "terminal", "arguments": string(args)}}}}
				finish = "tool_calls"
			}
		}
		if reasoning {
			message["reasoning_content"] = reasoningMarker
		}
		if body.Stream {
			if calls, ok := message["tool_calls"].([]any); ok {
				calls[0].(map[string]any)["index"] = 0
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range []map[string]any{
				{"index": 0, "delta": message, "finish_reason": nil},
				{"index": 0, "delta": map[string]any{}, "finish_reason": finish},
			} {
				data, _ := json.Marshal(map[string]any{"id": "aries-hermes-test", "object": "chat.completion.chunk", "created": 1, "model": "aries-deterministic", "choices": []any{chunk}})
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "aries-hermes-test", "object": "chat.completion", "created": 1, "model": "aries-deterministic", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20}})
	})
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: model, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	output := t.TempDir()
	provider, err := dockerdeployment.New(dockerdeployment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := tasksandbox.New(tasksandbox.Options{Deployment: provider, NewEnvironment: provider.NewTaskEnvironment, OutputDir: output})
	if err != nil {
		_ = provider.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sandbox.Close(); err != nil {
			t.Error(err)
		}
	})
	bridge, err := hermesssh.New(hermesssh.Options{ResolveListen: sandbox.BridgeListen, OutputDir: output})
	if err != nil {
		t.Fatal(err)
	}
	image := versions.Hermes.Image
	if len(derivedImage) > 0 {
		image = derivedImage[0]
	}
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

		Deployment:     integrationDeployment(t),
		Image:          image,
		OutputDir:      output,
		StartTimeout:   90 * time.Second,
		AgentTimeout:   90 * time.Second,
		CleanupTimeout: 45 * time.Second,
		APIKeyLookup:   func(string) ([]byte, bool) { return []byte(key), true },
	}})
	if err != nil {
		t.Fatal(err)
	}
	taskTimeout := 90 * time.Second
	if cancelCommand {
		taskTimeout = 8 * time.Second
	}
	harness := &runnerHermes{Manager: manager, modelPort: port}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := harness.Stop(cleanup); err != nil {
			t.Error(err)
		}
		if err := bridge.Stop(cleanup); err != nil {
			t.Error(err)
		}
		if err := manager.Close(); err != nil {
			t.Error(err)
		}
	})
	verifier := filepath.Join(t.TempDir(), "verifier.sh")
	if err := os.WriteFile(verifier, []byte("test \"$(cat /tmp/aries-hermes-proof)\" = ARIES_HERMES_BRIDGE_OK\n"), 0600); err != nil {
		t.Fatal(err)
	}
	benchmark := &hermesRunnerBenchmark{api: api, harness: harness, verifier: verifier, task: core.Task{ID: "hermes-bridge", Instruction: "Use the terminal tool to write ARIES_HERMES_BRIDGE_OK to /tmp/aries-hermes-proof, then report completion.", Timeout: taskTimeout, Environment: tasks[0].Environment}}
	modelConfig := core.ModelConfig{Provider: "openai", BaseURL: "http://127.0.0.1/v1", Model: "aries-deterministic", APIKeyEnv: "ARIES_TEST_MODEL_KEY"}
	if reasoning {
		modelConfig.Provider, modelConfig.Model, modelConfig.ReasoningEffort = "deepseek", "deepseek-flash", "high"
	}
	run, err := runner.New(benchmark, harness, sandbox, bridge, runner.Options{RunID: "hermes-runner-integration", OutputDir: output, Model: modelConfig, CleanupTimeout: 60 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for occurrence := 0; occurrence < repetitions; occurrence++ {
		// Match internal/app.nextTaskOccurrence: repeated logical task IDs get
		// distinct execution IDs, preserving every occurrence's evidence.
		benchmark.task.ID = fmt.Sprintf("hermes-bridge-%03d", occurrence+1)
		benchmark.evaluated = false
		result, err := run.Run(ctx)
		if err != nil && !cancelCommand {
			t.Fatalf("real Hermes Runner: %v; %#v", err, result)
		}
		if len(result.Tasks) != 1 {
			t.Fatalf("results: %#v", result)
		}
		task := result.Tasks[0]
		expectedStatus := core.StatusSucceeded
		if cancelCommand {
			expectedStatus = core.StatusCanceled
		}
		if task.Harness.Status != expectedStatus || task.Evaluation.Reward != 1 || !benchmark.evaluated || task.Isolation.Status != core.StatusConfirmed || task.Cleanup.Status != core.StatusSucceeded || toolResponses.Load() != int32(occurrence+1) {
			t.Fatalf("Runner task: %#v; tool calls %d", task, toolResponses.Load())
		}
		if reasoning && reasoningReplays.Load() != int32(occurrence+1) {
			t.Fatalf("reasoning replay count = %d", reasoningReplays.Load())
		}
		for _, id := range []string{harness.id, benchmark.containerID} {
			if _, err := api.ContainerInspect(ctx, id, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
				t.Errorf("container %s not absent: %v", id, err)
			}
		}
		if _, err := os.Stat(harness.identityPath); !os.IsNotExist(err) {
			t.Errorf("bridge credential remains: %v", err)
		}
		if _, err := api.NetworkInspect(ctx, benchmark.network, client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Errorf("network not absent: %v", err)
		}
		if !cancelCommand {
			if len(derivedImage) > 0 {
				assertToolSpan(t, filepath.Join(output, benchmark.task.ID, "harness", "telemetry", "otel-spans.jsonl"), "tool.terminal", "aries-terminal")
			}
			trajectory, err := os.ReadFile(filepath.Join(output, benchmark.task.ID, "harness", "telemetry", "sessions.jsonl"))
			if err != nil || !strings.Contains(string(trajectory), "ARIES_HERMES_BRIDGE_OK") {
				t.Fatalf("missing task trajectory: %v (%s)", err, trajectory)
			}
			if !strings.Contains(task.Harness.FinalResponse, "Completed the sandbox mutation") {
				t.Fatalf("final response = %q", task.Harness.FinalResponse)
			}
		}
		retained, err := os.ReadFile(filepath.Join(output, benchmark.task.ID, "harness", "config.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(retained), key) {
			t.Fatal("model credential leaked into retained configuration")
		}
	}
}
