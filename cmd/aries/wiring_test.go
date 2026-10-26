package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/internal/app"
	runtimesglang "github.com/hyscale-lab/aries/internal/modelruntime/sglang"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

func TestDispatchAcceptsOnlyExactCommandGrammar(t *testing.T) {
	runErr := errors.New("run")
	setupErr := errors.New("setup")
	for _, tc := range []struct {
		name        string
		args        []string
		wantRun     int
		wantSetup   int
		wantErr     error
		wantProfile string
	}{
		{name: "run", args: []string{"profile.json"}, wantRun: 1, wantErr: runErr, wantProfile: "profile.json"},
		{name: "setup", args: []string{"setup", "profile.json"}, wantSetup: 1, wantErr: setupErr, wantProfile: "profile.json"},
		{name: "empty"},
		{name: "profile named setup", args: []string{"setup"}, wantRun: 1, wantErr: runErr, wantProfile: "setup"},
		{name: "unknown two args", args: []string{"other", "profile.json"}},
		{name: "extra arg", args: []string{"setup", "profile.json", "extra"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runCalls, setupCalls := 0, 0
			var profile string
			run := func(context.Context, string, io.Writer, app.Dependencies) error {
				runCalls++
				profile = tc.args[0]
				return runErr
			}
			setup := func(_ context.Context, got string, _ io.Writer, _ app.Dependencies) error {
				setupCalls++
				profile = got
				return setupErr
			}
			err := dispatch(context.Background(), tc.args, &bytes.Buffer{}, app.Dependencies{}, run, setup)
			if runCalls != tc.wantRun || setupCalls != tc.wantSetup || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("run=%d setup=%d err=%v", runCalls, setupCalls, err)
			}
			if tc.wantErr == nil && (err == nil || err.Error() != "usage: aries PROFILE.json | aries setup PROFILE.json") {
				t.Fatalf("grammar err=%v", err)
			}
			if tc.wantProfile != "" && profile != tc.wantProfile {
				t.Fatalf("profile=%q", profile)
			}
		})
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

// Each bridge serves exactly one harness, so a crossed pair must be refused
// before the run starts rather than at the first tool call.
func TestValidateComponentsRequiresPairedHarnessAndBridge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		bridge  string
		wantErr bool
	}{
		{name: "openclaw pair", harness: "openclaw", bridge: "openclaw-ssh"},
		{name: "hermes pair", harness: "hermes", bridge: "hermes-ssh"},
		{name: "hermes gRPC pair", harness: "hermes", bridge: "hermes-grpc"},
		{name: "hermes with openclaw bridge", harness: "hermes", bridge: "openclaw-ssh", wantErr: true},
		{name: "openclaw with hermes bridge", harness: "openclaw", bridge: "hermes-ssh", wantErr: true},
		{name: "openclaw with hermes gRPC bridge", harness: "openclaw", bridge: "hermes-grpc", wantErr: true},
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
	bridge, err := newSharedBridge(cfg, "run-1", t.TempDir(), core.RuntimePlacement{}, nil)
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
