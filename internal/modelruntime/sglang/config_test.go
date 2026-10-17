package sglang

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const nativeConfig = `model-path: Qwen/Qwen3-8B
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

func TestSGLangNativeConfigAllowsNativeOptionsAndDefaults(t *testing.T) {
	withoutOptional := strings.NewReplacer(
		"context-length: 32768\n", "",
		"mem-fraction-static: 0.85\n", "",
		"reasoning-parser: qwen3\n", "",
		"tool-call-parser: qwen\n", "",
	).Replace(nativeConfig)
	for name, content := range map[string]string{
		"complete":          nativeConfig,
		"native options":    nativeConfig + "enable-metrics: true\nlog-requests: true\nquantization: fp8\n",
		"native defaults":   withoutOptional,
		"native values":     nativeConfig + "lora-paths: [adapter=/models/my-adapter]\njson-model-override-args: '{\"text\": \"--tp=2\"}'\n",
		"nested values":     nativeConfig + "json-model-override-args: {text: --tp=2}\n",
		"negative sentinel": nativeConfig + "chunked-prefill-size: -1\nexpert-distribution-recorder-buffer-size: -1\n",
		"quoted negative":   nativeConfig + "chunked-prefill-size: '-1'\n",
		"negative list":     nativeConfig + "custom-weight-loader: [-1, -0.5, '-.5']\n",
		"merged negative":   nativeConfig + "<<: {chunked-prefill-size: -1}\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1")
			if err != nil || cfg.Port != 30000 {
				t.Fatalf("cfg=%#v err=%v", cfg, err)
			}
		})
	}
}

func TestSGLangNativeConfigRejectsMeasurementAliases(t *testing.T) {
	base := strings.Replace(nativeConfig, "tensor-parallel-size: 1", "tensor-parallel-size: 2", 1)
	for _, option := range []string{
		"tp-size", "tp", "tp-s", "tensor-parallel-siz",
		"pp-size", "pp", "dp-size", "dp", "ep-size", "ep",
		"attn-cp-size", "moe-dp-size", "enable-dp-attentio",
		"served-model", "hos", "por", "dev", "nnode", "node-ran",
	} {
		t.Run(option, func(t *testing.T) {
			content := base + option + ": 1\n"
			if _, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1"); err == nil || !strings.Contains(err.Error(), option) {
				t.Fatalf("measurement alias %q must be rejected explicitly, got %v", option, err)
			}
		})
	}
	for name, content := range map[string]string{
		"alias first": "tp-size: 1\n" + base,
		"alias only":  strings.Replace(base, "tensor-parallel-size: 2", "tp-size: 1", 1),
		"yaml merge":  base + "<<: {tp-size: 1}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1"); err == nil || !strings.Contains(err.Error(), "tp-size") {
				t.Fatalf("alias bypassed measurement validation: %v", err)
			}
		})
	}
}

func TestSGLangNativeConfigRejectsArgumentOverrides(t *testing.T) {
	for name, content := range map[string]string{
		"full name with equals": nativeConfig + "tensor-parallel-size=2: true\n",
		"alias with equals":     nativeConfig + "tp=2: true\n",
		"merged key":            nativeConfig + "<<: {'tp=2': true}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1"); err == nil || !strings.Contains(err.Error(), "must not contain '='") {
				t.Fatalf("argument key bypassed validation: %v", err)
			}
		})
	}
	for name, content := range map[string]string{
		"scalar":         nativeConfig + "lora-target-modules: --tp=2\n",
		"list":           nativeConfig + "lora-target-modules: [q_proj, --tp=2]\n",
		"split list":     nativeConfig + "lora-target-modules: [--tp, 2]\n",
		"merged value":   nativeConfig + "<<: {lora-target-modules: [--tp=2]}\n",
		"aliased value":  nativeConfig + "json-model-override-args: {text: &arg --tp=2}\nlora-target-modules: [*arg]\n",
		"host":           strings.Replace(nativeConfig, "host: 0.0.0.0", "host: --tp=2", 1),
		"device":         strings.Replace(nativeConfig, "device: cuda", "device: --tp=2", 1),
		"served model":   strings.Replace(nativeConfig, "served-model-name: Qwen/Qwen3-8B", "served-model-name: --tp=2", 1),
		"numeric prefix": nativeConfig + "custom-weight-loader: [-1--tp=2]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1"); err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
				t.Fatalf("argument value bypassed validation: %v", err)
			}
		})
	}
}

func TestSGLangNativeConfigRejectsEveryInvalidLaunchValue(t *testing.T) {
	cases := map[string]string{
		"served model whitespace":  strings.Replace(nativeConfig, "served-model-name: Qwen/Qwen3-8B", "served-model-name: '   '", 1),
		"host whitespace":          strings.Replace(nativeConfig, "host: 0.0.0.0", "host: '   '", 1),
		"device whitespace":        strings.Replace(nativeConfig, "device: cuda", "device: '   '", 1),
		"port zero":                strings.Replace(nativeConfig, "port: 30000", "port: 0", 1),
		"port negative":            strings.Replace(nativeConfig, "port: 30000", "port: -1", 1),
		"port overflow":            strings.Replace(nativeConfig, "port: 30000", "port: 65536", 1),
		"tensor parallel zero":     strings.Replace(nativeConfig, "tensor-parallel-size: 1", "tensor-parallel-size: 0", 1),
		"tensor parallel negative": strings.Replace(nativeConfig, "tensor-parallel-size: 1", "tensor-parallel-size: -1", 1),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1"); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestSGLangNativeConfigRejectsInvalidDocumentsAndCrossChecks(t *testing.T) {
	tests := []struct {
		name, content, model, endpoint string
	}{
		{name: "malformed", content: "[", model: "Qwen/Qwen3-8B", endpoint: "http://host:30000/v1"},
		{name: "multiple documents", content: nativeConfig + "---\nhost: other\n", model: "Qwen/Qwen3-8B", endpoint: "http://host:30000/v1"},
		{name: "model mismatch", content: nativeConfig, model: "other", endpoint: "http://host:30000/v1"},
		{name: "port mismatch", content: nativeConfig, model: "Qwen/Qwen3-8B", endpoint: "http://host:30001/v1"},
		{name: "implicit port", content: nativeConfig, model: "Qwen/Qwen3-8B", endpoint: "http://host/v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadNativeConfig(writeNativeConfig(t, tc.content), tc.model, tc.endpoint); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestCheckedInSGLangNativeConfigLoads(t *testing.T) {
	path := filepath.Join("..", "..", "..", "configs", "sglang", "qwen3-8b-local.yaml")
	if _, err := LoadNativeConfig(path, "Qwen/Qwen3-8B", "http://127.0.0.1:30000/v1"); err != nil {
		t.Fatal(err)
	}
}

func TestSGLangGPUIndicesFollowLocalParallelWorkers(t *testing.T) {
	content := strings.Replace(nativeConfig, "tensor-parallel-size: 1", `tensor-parallel-size: 4
pipeline-parallel-size: 2
data-parallel-size: 2
enable-dp-attention: true
expert-parallel-size: 2
attention-context-parallel-size: 2
moe-data-parallel-size: 2
nnodes: 2
node-rank: 1`, 1)
	cfg, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1")
	if err != nil {
		t.Fatal(err)
	}
	indices, err := cfg.ResolveGPUIndices(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indices, []int{0, 1, 2, 3}) {
		t.Fatalf("default indices = %v", indices)
	}
	explicit, err := cfg.ResolveGPUIndices([]int{2, 4, 6, 7})
	if err != nil || !slices.Equal(explicit, []int{2, 4, 6, 7}) {
		t.Fatalf("explicit=%v err=%v", explicit, err)
	}
	if _, err := cfg.ResolveGPUIndices([]int{0, 1}); err == nil {
		t.Fatal("accepted GPU count mismatch")
	}
}

func TestSGLangStandardDataParallelismReplicatesWorkers(t *testing.T) {
	content := strings.Replace(nativeConfig, "tensor-parallel-size: 1", `tensor-parallel-size: 2
pipeline-parallel-size: 1
data-parallel-size: 3`, 1)
	cfg, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1")
	if err != nil {
		t.Fatal(err)
	}
	count, err := cfg.GPUCount()
	if err != nil || count != 6 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestSGLangNonCUDADeviceDoesNotSelectNVIDIAGPUs(t *testing.T) {
	content := strings.Replace(nativeConfig, "device: cuda", "device: cpu", 1)
	cfg, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1")
	if err != nil {
		t.Fatal(err)
	}
	indices, err := cfg.ResolveGPUIndices(nil)
	if err != nil || indices != nil {
		t.Fatalf("indices=%v err=%v", indices, err)
	}
	if _, err := cfg.ResolveGPUIndices([]int{0}); err == nil {
		t.Fatal("non-CUDA device accepted gpu_indices")
	}
}

func TestSGLangGPUCountRejectsUnsupportedLocalTopology(t *testing.T) {
	content := strings.Replace(nativeConfig, "tensor-parallel-size: 1", `tensor-parallel-size: 3
pipeline-parallel-size: 1
nnodes: 2`, 1)
	cfg, err := LoadNativeConfig(writeNativeConfig(t, content), "Qwen/Qwen3-8B", "http://host:30000/v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.GPUCount(); err == nil {
		t.Fatal("accepted indivisible multi-node topology")
	}
}

func writeNativeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "native.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
