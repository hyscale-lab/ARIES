package main

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
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

func TestExplicitCompositionSwitches(t *testing.T) {
	source, err := os.ReadFile("wiring.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "wiring.go", source, 0); err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, value := range []string{`case "terminalbench2"`, `case "openclaw"`, `case "docker"`, `case "openclaw-ssh"`, `case "deepseek"`, `case "sglang"`} {
		if !strings.Contains(text, value) {
			t.Fatalf("missing explicit switch %s", value)
		}
	}
	for _, forbidden := range []string{"plugin.Open", "reflect.", "Register("} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("framework selection found: %s", forbidden)
		}
	}
}

func TestValidateComponentsRejectsEveryUnsupportedSelector(t *testing.T) {
	base := config.Config{
		Benchmark: config.BenchmarkConfig{Type: "terminalbench2"},
		Harness:   config.HarnessConfig{Type: "openclaw"},
		Sandbox:   config.SandboxConfig{Type: "docker"},
		Bridge:    config.BridgeConfig{Type: "openclaw-ssh"},
	}
	for _, tc := range []struct {
		name string
		set  func(*config.Config)
		want string
	}{
		{name: "benchmark", set: func(cfg *config.Config) { cfg.Benchmark.Type = "other" }, want: "unsupported benchmark type"},
		{name: "harness", set: func(cfg *config.Config) { cfg.Harness.Type = "other" }, want: "unsupported harness type"},
		{name: "sandbox", set: func(cfg *config.Config) { cfg.Sandbox.Type = "other" }, want: "unsupported sandbox type"},
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

func TestMakeLintIncludesInternalPackages(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(makefile), "find cmd internal pkg") || !strings.Contains(string(makefile), "go vet ./...") {
		t.Fatalf("lint target does not cover cmd, internal, and pkg: %s", makefile)
	}
}

func TestExternalSGLangPreparationReturnsNilRuntime(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "native.yaml")
	if err := os.WriteFile(native, []byte(nativeForWiring), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Runtime: config.RuntimeConfig{Backend: "sglang", Mode: "external", Config: config.RuntimeConfigValues{ResolvedFile: native}}, Model: config.ProfileModel{ID: "Qwen/Qwen3-8B", BaseURL: "http://host:30000/v1", APIKeyEnv: "KEY"}}
	prepared, err := prepareBackend(cfg, filepath.Join(root, "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Runtime != nil || prepared.Model.Provider != "sglang" {
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
	indices, err := resolveRuntimeGPUIndices(cfg)
	if err != nil || len(indices) != 1 || indices[0] != 0 {
		t.Fatalf("GPU indices = %v, error = %v", indices, err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("preparation created output: %v", err)
	}
}

func TestPrepareBackendRejectsManagedSGLangGPUCountMismatch(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "native.yaml")
	content := strings.Replace(nativeForWiring, "tensor-parallel-size: 1", "tensor-parallel-size: 2", 1)
	if err := os.WriteFile(native, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "python")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Runtime: config.RuntimeConfig{Backend: "sglang", Mode: "managed", Config: config.RuntimeConfigValues{
			ResolvedFile: native, Executable: executable, GPUIndices: []int{0},
		}},
		Model: config.ProfileModel{ID: "Qwen/Qwen3-8B", BaseURL: "http://host:30000/v1", APIKeyEnv: "KEY"},
	}
	if _, err := prepareBackend(cfg, filepath.Join(root, "output")); err == nil || !strings.Contains(err.Error(), "requires 2") {
		t.Fatalf("error = %v", err)
	}
}

func TestManagedSGLangReceivesConfiguredCredentialEnvironmentName(t *testing.T) {
	source, err := os.ReadFile("wiring.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "CredentialEnv: cfg.Model.APIKeyEnv") {
		t.Fatal("managed SGLang credential environment name is not forwarded")
	}
}

func TestCombinedResourceSourceSamplesAndClosesBothSources(t *testing.T) {
	firstErr := errors.New("first close")
	secondErr := errors.New("second close")
	container := &wiringResourceSource{readings: []core.ResourceReading{{RuntimeID: "container"}}, closeErr: firstErr}
	gpu := &wiringResourceSource{readings: []core.ResourceReading{{RuntimeID: "gpu"}}, closeErr: secondErr}
	source := &combinedResourceSource{container: container, gpu: gpu}
	readings, err := source.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 || readings[0].RuntimeID != "container" || readings[1].RuntimeID != "gpu" {
		t.Fatalf("readings = %#v", readings)
	}
	if err := source.Close(); !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("close error = %v", err)
	}
	if container.closes != 1 || gpu.closes != 1 {
		t.Fatalf("closes = container %d gpu %d", container.closes, gpu.closes)
	}
}

type wiringResourceSource struct {
	readings []core.ResourceReading
	closeErr error
	closes   int
}

func (source *wiringResourceSource) Sample(context.Context) ([]core.ResourceReading, error) {
	return source.readings, nil
}

func (source *wiringResourceSource) Close() error {
	source.closes++
	return source.closeErr
}

func TestBackendPreparationRejectsNativeMismatchBeforeArtifacts(t *testing.T) {
	root := t.TempDir()
	native := filepath.Join(root, "native.yaml")
	if err := os.WriteFile(native, []byte(strings.Replace(nativeForWiring, "port: 30000", "port: 30001", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "output")
	cfg := config.Config{Runtime: config.RuntimeConfig{Backend: "sglang", Mode: "managed", Config: config.RuntimeConfigValues{ResolvedFile: native, Executable: "python3"}}, Model: config.ProfileModel{ID: "Qwen/Qwen3-8B", BaseURL: "http://host:30000/v1", APIKeyEnv: "KEY"}}
	if _, err := prepareBackend(cfg, output); err == nil {
		t.Fatal("expected mismatch")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output side effect: %v", err)
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
