//go:build integration

package codex_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/roadmapbench"
	"github.com/hyscale-lab/aries/pkg/bridge/codexssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	codexharness "github.com/hyscale-lab/aries/pkg/harness/codex"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const roadmapPrivateMarker = "ROADMAP_PRIVATE_VERIFIER_SENTINEL"

func TestRunnerRoadmapBenchThroughCodexNativeSSH(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(filepath.Join(root, "profiles", "codex-roadmapbench-qwen38-27b-xhigh-first20.json"))
	if err != nil {
		t.Fatal(err)
	}
	binary := integrationExecutable(t, "ARIES_CODEX_BINARY", filepath.Join(root, ".cache/codex/0.157.1/codex"))
	helper := integrationExecutable(t, "ARIES_CODEX_SSH_CLIENT", filepath.Join(root, "bin/aries-codex-ssh"))
	supervisor := integrationExecutable(t, "ARIES_CODEX_EXEC_SUPERVISOR", filepath.Join(root, "bin/aries-codex-exec"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := dockersandbox.PullImages(ctx, []string{integrationImage}); err != nil {
		t.Fatal(err)
	}
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })

	for _, test := range []struct {
		name   string
		reward float64
		status string
	}{
		{"partial", 0.5, core.StatusFailed},
		{"complete", 1, core.StatusSucceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := t.TempDir()
			logger := logrus.New()
			logger.SetOutput(io.Discard)
			runID := fmt.Sprintf("codex-roadmap-%d", time.Now().UnixNano())
			// Registered before component cleanup, so this also checks aborted runs.
			t.Cleanup(func() {
				cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				filters := client.Filters{}.Add("label", "aries.run="+runID)
				containers, err := api.ContainerList(cleanup, client.ContainerListOptions{All: true, Filters: filters})
				if err != nil || len(containers.Items) != 0 {
					t.Errorf("owned container leak count=%d error=%v", len(containers.Items), err)
				}
				networks, err := api.NetworkList(cleanup, client.NetworkListOptions{Filters: filters})
				if err != nil || len(networks.Items) != 0 {
					t.Errorf("owned network leak count=%d error=%v", len(networks.Items), err)
				}
			})
			fixtureRoot, revision := roadmapFixture(t)
			const logicalID = "opt-3.0.0-roadmap"
			const executionID = logicalID + "-001"
			benchmark, err := roadmapbench.New(roadmapbench.Options{Root: fixtureRoot, Revision: revision, OutputDir: output, TaskIDs: []string{logicalID}, ExecutionTaskIDs: []string{executionID}})
			if err != nil {
				t.Fatal(err)
			}
			sandboxes, err := dockersandbox.New(dockersandbox.Options{OutputDir: output, Logger: logger})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sandboxes.Close() })
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
			manager, err := codexharness.New(codexharness.Options{
				Image: integrationImage, CodexPath: binary, CodexVersion: "0.157.1", OutputDir: output, Logger: logger, StartTimeout: 90 * time.Second,
				APIKeyLookup:    func(string) ([]byte, bool) { return []byte(integrationKey), true },
				ReasoningEffort: cfg.Harness.Codex.ReasoningEffort, DeveloperInstructions: cfg.Harness.Codex.DeveloperInstructions,
				SubagentsEnabled: *cfg.Harness.Subagents.Enabled, MaxConcurrentSubagents: cfg.Harness.Subagents.MaxConcurrent,
			})
			if err != nil {
				t.Fatal(err)
			}
			model := &responsesFixture{
				subagents: true, developerInstructions: cfg.Harness.Codex.DeveloperInstructions,
				privateMarker: roadmapPrivateMarker,
				command:       `set -eu; test "$PWD" = /app; test -z "${ARIES_CODEX_TEST_KEY+x}"; test ! -e /run/aries/codex/model.key; test ! -e /tests; test ! -e /solution; test ! -e /logs/verifier; printf ` + test.name + ` > /app/roadmap-feature; printf REMOTE_CODEX_OK`,
			}
			harness := &roadmapModelHarness{Manager: manager, model: model}
			t.Cleanup(func() {
				cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				if err := harness.Stop(cleanup); err != nil {
					t.Errorf("harness cleanup: %v", err)
				}
				_ = manager.Close()
			})
			run, err := runner.New(benchmark, harness, sandboxes, bridge, runner.Options{
				RunID: runID, OutputDir: output, Logger: logger,
				Model: core.ModelConfig{Provider: "openai", BaseURL: "http://fixture.invalid/v1", Model: cfg.Model.ID, APIKeyEnv: "ARIES_CODEX_TEST_KEY"},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := run.Run(ctx)
			if err != nil || len(result.Tasks) != 1 {
				t.Fatalf("Codex RoadmapBench run: %+v, %v", result, err)
			}
			got := result.Tasks[0]
			if got.TaskID != executionID || got.Harness.Status != core.StatusSucceeded || got.Harness.FinalResponse != "native Codex SSH complete" || got.Isolation.Status != core.StatusConfirmed || !got.Isolation.HarnessStopped || !got.Isolation.BridgeRevoked || got.Cleanup.Status != core.StatusSucceeded {
				t.Fatalf("incomplete task lifecycle: %+v", got)
			}
			if got.Evaluation.Status != test.status || got.Evaluation.VerifierStatus != test.status || got.Evaluation.Score != test.reward || got.Evaluation.Reward != test.reward || got.Evaluation.Error != "" {
				t.Fatalf("RoadmapBench reward = %+v", got.Evaluation)
			}
			model.mu.Lock()
			calls, modelErr := model.calls, model.err
			model.mu.Unlock()
			if calls != 6 || modelErr != nil {
				t.Fatalf("native Responses calls=%d error=%v", calls, modelErr)
			}
			if connection, err := net.DialTimeout("tcp", harness.endpoint.Address, 200*time.Millisecond); err == nil {
				connection.Close()
				t.Fatal("revoked bridge still accepts connections")
			}
			if _, err := os.Stat(harness.endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("bridge identity retained after evaluation: %v", err)
			}
			artifacts := append(append([]string{}, got.Harness.LogPaths...), got.ToolLogPaths...)
			artifacts = append(artifacts, got.Evaluation.LogPaths...)
			for _, name := range artifacts {
				content, err := os.ReadFile(name)
				if err != nil || bytes.Contains(content, []byte(integrationKey)) {
					t.Fatalf("invalid or credential-bearing artifact %s: %v", name, err)
				}
				info, err := os.Stat(name)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("artifact is not private: %s", name)
				}
			}
			trajectory, err := os.ReadFile(filepath.Join(output, executionID, "harness", "trajectory.jsonl"))
			if err != nil || !bytes.Contains(trajectory, []byte(`"type":"command_execution"`)) || !bytes.Contains(trajectory, []byte(`"type":"turn.completed"`)) {
				t.Fatalf("missing native tool/turn evidence: %s, %v", trajectory, err)
			}
			if !bytes.Contains(trajectory, []byte(`"tool":"spawn_agent"`)) || !bytes.Contains(trajectory, []byte(`"tool":"wait"`)) {
				t.Fatalf("missing completed native delegation: %s", trajectory)
			}
		})
	}
}

// Bind the deterministic endpoint on the actual task network once Runner has
// started its bridge, keeping normal Runner ownership and ordering intact.
type roadmapModelHarness struct {
	*codexharness.Manager
	model    *responsesFixture
	server   *http.Server
	endpoint core.ToolEndpoint
}

func (h *roadmapModelHarness) Start(ctx context.Context, request core.HarnessRequest) error {
	h.endpoint = request.Endpoint
	gateway, _, err := net.SplitHostPort(request.Endpoint.Address)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return err
	}
	h.server = &http.Server{Handler: h.model, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = h.server.Serve(listener) }()
	request.Model.BaseURL = "http://" + listener.Addr().String() + "/v1"
	return h.Manager.Start(ctx, request)
}

func (h *roadmapModelHarness) Stop(ctx context.Context) error {
	err := h.Manager.Stop(ctx)
	if h.server != nil {
		err = errors.Join(err, h.server.Close())
	}
	return err
}

func roadmapFixture(t *testing.T) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	files := map[string]string{
		"task.toml": `version = "1"
[agent]
timeout_sec = 90
[verifier]
timeout_sec = 30
[environment]
docker_image = "` + integrationImage + `"
build_timeout_sec = 60
cpus = 1
memory_mb = 1024
storage_mb = 1024
`,
		"instruction.md":         "Implement the roadmap feature in /app/roadmap-feature, then report completion.",
		"environment/Dockerfile": "FROM " + integrationImage + "\nWORKDIR /app\n",
		"tests/test.sh": `#!/bin/bash
set -eu
# ` + roadmapPrivateMarker + `
for stage in /.aries-codex-*; do test ! -e "$stage" && test ! -L "$stage"; done
case "$(cat /app/roadmap-feature)" in
  partial) reward=0.5 ;;
  complete) reward=1 ;;
  *) exit 1 ;;
esac
printf '%s\n' "$reward" > /logs/verifier/reward.txt
printf '{"reward":%s}\n' "$reward" > /logs/verifier/reward.json
`,
	}
	for name, content := range files {
		filename := filepath.Join(root, "opt-3.0.0-roadmap", name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "."}, {"-c", "user.name=ARIES Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "RoadmapBench fixture"}} {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("prepare pinned fixture: %v: %s", err, output)
		}
	}
	revision, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return root, strings.TrimSpace(string(revision))
}
