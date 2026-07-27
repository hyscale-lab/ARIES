package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runtimesglang "github.com/hyscale-lab/aries/internal/modelruntime/sglang"
	"github.com/hyscale-lab/aries/pkg/config"
)

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

func TestRunCommandSelectsManagedSGLangRuntime(t *testing.T) {
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
	if _, ok := prepared.Runtime.(*runtimesglang.Runtime); !ok {
		t.Fatalf("runtime type = %T", prepared.Runtime)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("preparation created output: %v", err)
	}
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
