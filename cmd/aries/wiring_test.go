package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/internal/app"
	runtimesglang "github.com/hyscale-lab/aries/internal/modelruntime/sglang"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	codexharness "github.com/hyscale-lab/aries/pkg/harness/codex"
)

func TestReplayWiringRejectsUnattachedCallerWithoutDockerEffects(t *testing.T) {
	cfg := config.Config{
		Benchmark: config.BenchmarkConfig{Type: "terminalbench2"},
		Replay:    &config.ReplayConfig{Source: "source-run/task-001"},
		Bridge:    config.BridgeConfig{Type: "hermes-grpc"},
		Versions:  config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}},
	}
	wiring := commandWiring()
	if wiring.NewReplayCaller == nil || wiring.PreflightReplaySource == nil {
		t.Fatal("command wiring has no replay caller or source preflight")
	}
	if _, err := wiring.NewReplayCaller(cfg, core.RuntimePlacement{}, 0, nil); err == nil {
		t.Fatal("caller without private run attachment was accepted")
	}
}

func TestValidateComponentsAllowsOnlyReplayGRPCAndPrivateRoute(t *testing.T) {
	cfg := config.Config{
		Benchmark: config.BenchmarkConfig{Type: "terminalbench2"},
		Replay:    &config.ReplayConfig{Source: "source-run/task-001"},
		Bridge:    config.BridgeConfig{Type: "hermes-grpc"},
		Versions:  config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}},
	}
	if err := validateComponents(cfg); err != nil {
		t.Fatalf("valid replay components rejected: %v", err)
	}
	cfg.Benchmark.Type = "toolathlon"
	if err := validateComponents(cfg); err == nil {
		t.Fatal("accepted Toolathlon replay")
	}
	cfg.Benchmark.Type = "terminalbench2"
	cfg.Bridge.Type = "hermes-ssh"
	if err := validateComponents(cfg); err == nil {
		t.Fatal("accepted replay SSH bridge")
	}
	cfg.Bridge.Type = "hermes-grpc"
	cfg.Replay.Deployment = config.DeploymentConfig{Backend: "docker", Docker: &config.DockerDeploymentConfig{Socket: "/other/docker.sock"}}
	if err := validateComponents(cfg); err == nil {
		t.Fatal("accepted caller on another Docker daemon")
	}
}

func TestCLIFlags(t *testing.T) {
	const (
		exitSuccess   = 0
		exitUsage     = 1
		exitFlagError = 2
	)
	dir := t.TempDir()
	binary := filepath.Join(dir, "aries")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aries: %v\n%s", err, output)
	}
	profile := filepath.Join(dir, "missing.json")
	for _, tc := range []struct {
		name       string
		args       []string
		wantExit   int
		wantOutput string
	}{
		{"help", []string{"--help"}, exitSuccess, "-record-for-replay"},
		{"unknown leading flag", []string{"--unknown", profile}, exitFlagError, "flag provided but not defined"},
		{"flag after profile", []string{profile, "--record-for-replay"}, exitUsage, "usage: aries"},
		{"recording with setup", []string{"--record-for-replay", "setup", profile}, exitUsage, "usage: aries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), binary, tc.args...)
			cmd.Env = slices.DeleteFunc(cmd.Environ(), func(entry string) bool {
				return strings.HasPrefix(entry, "ARIES_RUN_NAMESPACE=")
			})
			output, err := cmd.CombinedOutput()
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("start aries: %v", err)
				}
			}
			if code := cmd.ProcessState.ExitCode(); code != tc.wantExit {
				t.Errorf("exit code = %d, want %d\n%s", code, tc.wantExit, output)
			}
			if !strings.Contains(string(output), tc.wantOutput) {
				t.Errorf("output = %q, want %q", output, tc.wantOutput)
			}
		})
	}
}

func TestDispatch(t *testing.T) {
	const profile = "profile.json"
	runErr := errors.New("run")
	setupErr := errors.New("setup")
	for _, tc := range []struct {
		name        string
		args        []string
		record      bool
		wantErr     error
		wantProfile string
	}{
		{name: "run", args: []string{profile}, wantErr: runErr, wantProfile: profile},
		{name: "record", args: []string{profile}, record: true, wantErr: runErr, wantProfile: profile},
		{name: "setup", args: []string{"setup", profile}, wantErr: setupErr, wantProfile: profile},
		{name: "profile named setup", args: []string{"setup"}, wantErr: runErr, wantProfile: "setup"},
		{name: "empty"},
		{name: "unknown two args", args: []string{"other", profile}},
		{name: "extra arg", args: []string{"setup", profile, "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(result error) useCase {
				return func(_ context.Context, path string, _ io.Writer, deps app.Dependencies) error {
					if result != tc.wantErr || path != tc.wantProfile || deps.RecordForReplay != tc.record {
						t.Fatalf("called %v with profile %q and recording=%v", result, path, deps.RecordForReplay)
					}
					return result
				}
			}
			deps := app.Dependencies{RecordForReplay: tc.record}
			err := dispatch(t.Context(), tc.args, io.Discard, deps, check(runErr), check(setupErr))
			if tc.wantErr == nil {
				if err == nil || !strings.HasPrefix(err.Error(), "usage: aries") {
					t.Fatalf("error = %v, want usage", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestApptainerNamespaceForwardsArgs(t *testing.T) {
	t.Setenv("ARIES_RUN_NAMESPACE", "")
	profile := filepath.Join("..", "..", "profiles", "apptainer", "hermes-tb2-fix-git-apptainer.json")
	args := []string{"--record-for-replay=false", "--", profile}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var entered []string
	enter := func(_ context.Context, argv []string) (int, error) {
		entered = argv
		return 0, nil
	}
	code, selected, err := runInOwnNamespace(t.Context(), args, []string{profile}, enter)
	if want := append([]string{executable}, args...); err != nil || !selected || code != 0 || !slices.Equal(entered, want) {
		t.Fatalf("code=%d selected=%v argv=%q want=%q err=%v", code, selected, entered, want, err)
	}
}

func TestValidateComponentsRejectsEveryUnsupportedSelector(t *testing.T) {
	base := config.Config{
		Benchmark: config.BenchmarkConfig{Type: "terminalbench2"},
		Harness:   config.HarnessConfig{Type: "openclaw"},
		Sandbox:   config.SandboxConfig{Type: "docker"},
		Bridge:    config.BridgeConfig{Type: "openclaw-ssh"},
		Versions:  config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}},
	}
	for _, tc := range []struct {
		name string
		set  func(*config.Config)
		want string
	}{
		{name: "benchmark", set: func(cfg *config.Config) { cfg.Benchmark.Type = "other" }, want: "unsupported benchmark type"},
		{name: "harness", set: func(cfg *config.Config) { cfg.Harness.Type = "other" }, want: "unsupported harness type"},
		{name: "sandbox", set: func(cfg *config.Config) { cfg.Sandbox.Type = "other" }, want: "sandbox.type: unsupported legacy value"},
		{name: "bridge", set: func(cfg *config.Config) { cfg.Bridge.Type = "other" }, want: "unsupported bridge type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.set(&cfg)
			if err := validateComponents(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateComponentsAcceptsSWEAtlasQA(t *testing.T) {
	cfg := config.Config{
		Benchmark: config.BenchmarkConfig{Type: "sweatlasqa"},
		Harness:   config.HarnessConfig{Type: "openclaw"},
		Sandbox:   config.SandboxConfig{Type: "docker"},
		Bridge:    config.BridgeConfig{Type: "openclaw-ssh"},
		Versions:  config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}},
	}
	if err := validateComponents(cfg); err != nil {
		t.Fatalf("err=%v", err)
	}
}

func TestBenchmarkDispatchersFailClosed(t *testing.T) {
	cfg := config.Config{Benchmark: config.BenchmarkConfig{Type: "unsupported"}}
	if _, err := benchmarkForTasks(cfg, t.TempDir(), []string{"task"}, nil, nil); err == nil || !strings.Contains(err.Error(), "unsupported benchmark type") {
		t.Fatalf("benchmarkForTasks error = %v", err)
	}
	if err := setupBenchmark(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "unsupported benchmark type") {
		t.Fatalf("setupBenchmark error = %v", err)
	}
	if _, err := loadBenchmark(context.Background(), cfg, t.TempDir(), []string{"task"}, nil); err == nil || !strings.Contains(err.Error(), "unsupported benchmark type") {
		t.Fatalf("loadBenchmark error = %v", err)
	}
}

// One run-shared bridge serves several harness instances at a time, all of one
// harness type: `openclaw-ssh` serves only OpenClaw, and `hermes-ssh` or
// `hermes-grpc` serves only Hermes. Thus a crossed pair must be refused before
// the run starts rather than at the first tool call.
func TestValidateComponentsRequiresPairedHarnessAndBridge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		bridge  string
		wantErr bool
	}{
		{name: "openclaw pair", harness: "openclaw", bridge: "openclaw-ssh"},
		{name: "hermes pair", harness: "hermes", bridge: "hermes-ssh"},
		{name: "codex pair", harness: "codex", bridge: "codex-ssh"},
		{name: "hermes with openclaw bridge", harness: "hermes", bridge: "openclaw-ssh", wantErr: true},
		{name: "openclaw with hermes bridge", harness: "openclaw", bridge: "hermes-ssh", wantErr: true},
		{name: "codex with hermes bridge", harness: "codex", bridge: "hermes-ssh", wantErr: true},
		{name: "openclaw with codex bridge", harness: "openclaw", bridge: "codex-ssh", wantErr: true},
		{name: "hermes gRPC pair", harness: "hermes", bridge: "hermes-grpc"},
		{name: "openclaw with hermes gRPC bridge", harness: "openclaw", bridge: "hermes-grpc", wantErr: true},
		{name: "codex with hermes gRPC bridge", harness: "codex", bridge: "hermes-grpc", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Benchmark: config.BenchmarkConfig{Type: "terminalbench2"},
				Harness:   config.HarnessConfig{Type: tc.harness},
				Sandbox:   config.SandboxConfig{Type: "docker"},
				Bridge:    config.BridgeConfig{Type: tc.bridge},
				Versions:  config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}},
			}
			err := validateComponents(cfg)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "paired bridge") {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCodexHarnessConstructionUsesSelectedNativeVersion(t *testing.T) {
	cfg := config.Config{
		Harness: config.HarnessConfig{
			Type: "codex", Mode: "agent",
			Codex: &config.HarnessCodexConfig{ResolvedExecutable: filepath.Join(t.TempDir(), "codex")},
		},
		Bridge:   config.BridgeConfig{Type: "codex-ssh"},
		Versions: config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}, Codex: config.CodexVersions{Image: "docker.io/library/debian:12.12-slim", Version: "0.157.1"}},
	}
	instance, err := newHarness(cfg, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := instance.Harness.(*codexharness.Manager); !ok {
		t.Fatalf("harness = %T, want Codex", instance.Harness)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.Versions.Codex.Version = "0.156.1"
	if _, err := newHarness(cfg, t.TempDir(), nil, nil); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("unsupported native version was accepted: %v", err)
	}
}

// Every checked-in profile must pass the component preflight, not only load.
func TestCheckedInProfilesPassComponentValidation(t *testing.T) {
	root := filepath.Join("..", "..", "profiles")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".json" || strings.HasSuffix(path, "-overrides.json") {
			return err
		}
		t.Run(strings.TrimPrefix(path, root+string(filepath.Separator)), func(t *testing.T) {
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateComponents(cfg); err != nil {
				t.Fatal(err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The run builds its bridge for every bridge type that preflight accepts.
func TestNewSharedBridgeAcceptsTheHermesGRPCBridge(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "profiles", "hermes-tb2-fix-git-grpc-deepseek.json"))
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := newSharedBridge(cfg, "run-1", t.TempDir(), core.RuntimePlacement{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestExternalSGLangPreparationReturnsNilRuntime(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{Runtime: config.RuntimeConfig{Backend: "sglang", Mode: "external"}, Model: config.ProfileModel{ID: "Qwen/Qwen3-8B", BaseURL: "http://host:30000/v1", APIKeyEnv: "KEY"}}
	prepared, err := prepareBackend(cfg, filepath.Join(root, "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Runtime != nil || prepared.Model.Provider != "sglang" || len(prepared.EffectiveGPUIndices) != 0 {
		t.Fatalf("prepared=%#v", prepared)
	}
	if _, err := os.Stat(filepath.Join(root, "absent")); !os.IsNotExist(err) {
		t.Fatalf("preparation created output: %v", err)
	}
}

func TestPrepareBackendSelectsManagedSGLangRuntime(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "native config.yaml")
	if err := os.WriteFile(native, []byte(nativeForWiring), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "python helper")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "not-created")
	cfg := config.Config{
		Runtime: config.RuntimeConfig{Backend: "sglang", Mode: "managed", Config: config.RuntimeConfigValues{ResolvedFile: native, Executable: executable, StartupTimeout: time.Minute, StopTimeout: time.Second}},
		Model:   config.ProfileModel{ID: "Qwen/Qwen3-8B", BaseURL: "http://host:30000/v1", APIKeyEnv: "KEY"},
	}
	prepared, err := prepareBackend(cfg, output)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := prepared.Runtime.(*runtimesglang.Runtime)
	if !ok {
		t.Fatalf("runtime type = %T", prepared.Runtime)
	}
	if !reflect.DeepEqual(prepared.EffectiveGPUIndices, []int{0}) {
		t.Fatalf("GPU indices = %v", prepared.EffectiveGPUIndices)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("preparation created output: %v", err)
	}
}

const nativeForWiring = `model-path: Qwen/Qwen3-8B
served-model-name: Qwen/Qwen3-8B
host: 0.0.0.0
port: 30000
device: cuda
tensor-parallel-size: 1
context-length: 32768
mem-fraction-static: 0.85
reasoning-parser: qwen3
tool-call-parser: qwen
`

func TestExternalOpenAIPreparationReturnsNilRuntime(t *testing.T) {
	cfg := config.Config{Runtime: config.RuntimeConfig{Backend: "openai", Mode: "external"}, Model: config.ProfileModel{ID: "served/model", BaseURL: "http://vllm.local:8000/v1", APIKeyEnv: "KEY"}}
	prepared, err := prepareBackend(cfg, filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Runtime != nil || prepared.Model.Provider != "openai" || len(prepared.EffectiveGPUIndices) != 0 {
		t.Fatalf("prepared=%#v", prepared)
	}
	cfg.Runtime.Mode = "managed"
	if _, err := prepareBackend(cfg, t.TempDir()); err == nil {
		t.Fatal("managed OpenAI-compatible runtime was accepted")
	}
}

func TestNewHarnessRejectsInvalidMCPCredentials(t *testing.T) {
	outputDir := t.TempDir()
	lookup := func(string) ([]byte, bool) { return []byte("test-key"), true }
	invalidServers := []core.MCPServerConfig{
		{Name: "bad", Command: "mcp-server", SecretEnv: map[string]string{"SECRET": "invalid-secret-value!"}},
	}
	for _, harnessType := range []string{"openclaw", "hermes"} {
		t.Run(harnessType+"_invalid", func(t *testing.T) {
			cfg := config.Config{
				Harness: config.HarnessConfig{
					Type:       harnessType,
					MCPServers: invalidServers,
				},
				Versions: config.Versions{
					OpenClaw: config.OpenClawVersions{Image: "ghcr.io/openclaw/openclaw:2026.7.1"},
					Hermes:   config.HermesVersions{Image: "docker.io/nousresearch/hermes-agent:v2026.8.31"},
				},
			}
			if _, err := newHarness(cfg, outputDir, lookup, nil); err == nil {
				t.Fatalf("newHarness(%s) accepted invalid MCPServers", harnessType)
			}
		})
	}
}
