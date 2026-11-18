// Package runtime constructs the model services selected by the command layer.
package runtime

import (
	"context"
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	runtimesglang "github.com/hyscale-lab/aries/internal/modelruntime/sglang"
	"github.com/hyscale-lab/aries/pkg/config"
)

// ManagedSGLangLeftovers lists the managed SGLang server a run left running,
// if the profile has ARIES start one. It is stopped as a normal run stops it,
// with the profile's stop_timeout before SIGKILL.
func ManagedSGLangLeftovers(cfg config.Config, outputRoot string) ([]app.Leftover, error) {
	if cfg.Runtime.Backend != "sglang" || cfg.Runtime.Mode != "managed" {
		return nil, nil
	}
	pid, running, err := runtimesglang.RecordedServer(outputRoot)
	if err != nil || !running {
		return nil, err
	}
	grace := cfg.Runtime.Config.StopTimeout
	return []app.Leftover{{
		Kind: "model server", Name: fmt.Sprintf("SGLang process group %d", pid),
		Remove: func(ctx context.Context) error { return runtimesglang.StopRecordedServer(ctx, outputRoot, grace) },
	}}, nil
}

// NewSGLang validates native configuration and prepares a managed runtime.
// Construction starts no process and creates no output artifacts.
func NewSGLang(cfg config.Config, outputDir string) (app.PreparedBackend, error) {
	native, err := runtimesglang.LoadNativeConfig(cfg.Runtime.Config.ResolvedFile, cfg.Model.ID, cfg.Model.BaseURL)
	if err != nil {
		return app.PreparedBackend{}, err
	}
	gpuIndices, err := native.ResolveGPUIndices(cfg.Runtime.Config.GPUIndices)
	if err != nil {
		return app.PreparedBackend{}, fmt.Errorf("resolve managed SGLang GPUs: %w", err)
	}
	runtime, err := runtimesglang.New(runtimesglang.Options{Executable: cfg.Runtime.Config.Executable, ConfigPath: cfg.Runtime.Config.ResolvedFile, OutputDir: outputDir, BaseURL: cfg.Model.BaseURL, CredentialEnv: cfg.Model.APIKeyEnv, GPUIndices: append([]int(nil), gpuIndices...)})
	if err != nil {
		return app.PreparedBackend{}, err
	}
	return app.PreparedBackend{Model: cfg.CoreModel(), Runtime: runtime, EffectiveGPUIndices: append([]int(nil), gpuIndices...)}, nil
}
