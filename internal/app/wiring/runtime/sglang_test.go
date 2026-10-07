package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestPrepareBackendPreservesExplicitGPUOrderAndOwnership(t *testing.T) {
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
			ResolvedFile: native, Executable: executable, GPUIndices: []int{4, 2},
		}},
		Model: config.ProfileModel{ID: "Qwen/Qwen3-8B", BaseURL: "http://host:30000/v1", APIKeyEnv: "KEY"},
	}
	prepared, err := NewSGLang(cfg, filepath.Join(root, "output"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runtime.Config.GPUIndices[0] = 99
	if !reflect.DeepEqual(prepared.EffectiveGPUIndices, []int{4, 2}) {
		t.Fatalf("prepared GPU indices = %v", prepared.EffectiveGPUIndices)
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
	if _, err := NewSGLang(cfg, filepath.Join(root, "output")); err == nil || !strings.Contains(err.Error(), "requires 2") {
		t.Fatalf("error = %v", err)
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
	if _, err := NewSGLang(cfg, output); err == nil {
		t.Fatal("expected mismatch")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("output side effect: %v", err)
	}
}

func TestManagedSGLangReceivesConfiguredCredentialEnvironmentName(t *testing.T) {
	t.Setenv("ARIES_WIRING_KEY", "synthetic-secret")
	root := t.TempDir()
	native := filepath.Join(root, "native.yaml")
	if err := os.WriteFile(native, []byte(nativeForWiring), 0600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "python-helper")
	script := "#!/bin/sh\nif [ \"${ARIES_WIRING_KEY+x}\" = x ]; then echo retained; else echo removed; fi\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Runtime: config.RuntimeConfig{Backend: "sglang", Mode: "managed", Config: config.RuntimeConfigValues{ResolvedFile: native, Executable: executable}},
		Model:   config.ProfileModel{ID: "Qwen/Qwen3-8B", BaseURL: "http://127.0.0.1:30000/v1", APIKeyEnv: "ARIES_WIRING_KEY"},
	}
	prepared, err := NewSGLang(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := prepared.Runtime.Stop(ctx); err != nil {
			var classified interface{ CleanupFailed() bool }
			if !errors.As(err, &classified) || classified.CleanupFailed() || !errors.Is(err, prepared.Runtime.Err()) {
				t.Errorf("runtime cleanup: %v", err)
			}
		}
	})
	if err := prepared.Runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-prepared.Runtime.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("runtime helper did not exit")
	}
	output, err := os.ReadFile(filepath.Join(root, "sglang", "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "removed\n" {
		t.Fatalf("configured credential reached runtime process: %q", output)
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
