// Package runtime constructs the model services selected by the command layer.
package runtime

import (
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	runtimesglang "github.com/hyscale-lab/aries/internal/modelruntime/sglang"
	"github.com/hyscale-lab/aries/pkg/config"
)

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
