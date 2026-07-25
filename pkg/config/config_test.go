package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadRuntimeOverridesStrictSparseAndChecked(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	valid := write("valid.json", `{"harness_resources":{"cpu":1.25,"memory_mb":1024},"agent_sandbox_resources":{"cpu":2.5,"memory_mb":4096},"agent_timeout_seconds":12.5}`)
	overrides, err := LoadRuntimeOverrides(valid)
	if err != nil {
		t.Fatal(err)
	}
	if overrides.HarnessResources.CPU == nil || *overrides.HarnessResources.CPU != 1.25 || overrides.AgentSandboxResources.CPU == nil || *overrides.AgentSandboxResources.CPU != 2.5 || overrides.AgentSandboxResources.MemoryMB == nil || *overrides.AgentSandboxResources.MemoryMB != 4096 || overrides.AgentTimeout == nil || *overrides.AgentTimeout != 12500_000_000 {
		t.Fatalf("overrides = %#v", overrides)
	}
	for name, content := range map[string]string{
		"unknown-top-level": `{"future":1}`, "unknown-nested": `{"harness_resources":{"future":1}}`, "trailing": `{} {}`, "zero-cpu": `{"agent_sandbox_resources":{"cpu":0}}`,
		"large-memory": `{"harness_resources":{"memory_mb":8796093022208}}`, "zero-timeout": `{"agent_timeout_seconds":0}`,
		"cpu-overflow": `{"harness_resources":{"cpu":1e999}}`, "timeout-overflow": `{"agent_timeout_seconds":1e999}`,
		"cpu-nan": `{"harness_resources":{"cpu":NaN}}`, "cpu-infinity": `{"harness_resources":{"cpu":Infinity}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadRuntimeOverrides(write(name+".json", content)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	threshold := math.Exp2(63) / 1e9
	for _, cpu := range []float64{threshold, math.Nextafter(threshold, math.Inf(1))} {
		path := write("cpu-threshold.json", fmt.Sprintf(`{"agent_sandbox_resources":{"cpu":%g}}`, cpu))
		if _, err := LoadRuntimeOverrides(path); err == nil {
			t.Fatalf("accepted CPU %g", cpu)
		}
	}
	below := math.Nextafter(threshold, 0)
	if below*1e9 >= math.Exp2(63) {
		t.Fatal("test threshold premise failed")
	}
	if _, err := LoadRuntimeOverrides(write("cpu-below.json", fmt.Sprintf(`{"agent_sandbox_resources":{"cpu":%g}}`, below))); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeExecutionDefaultsAndValidation(t *testing.T) {
	base := `{"name":"test","versions_file":"versions.json","benchmark":{"type":"terminalbench2","root":"root","tasks":["fix-git"]},"harness":{"type":"openclaw"},"sandbox":{"type":"docker"},"bridge":{"type":"openclaw-ssh"},"model":{"provider":"deepseek","base_url":"https://example.com","model":"model","api_key_env":"KEY"},"output_dir":"runs"}`
	cfg, err := Decode(strings.NewReader(base))
	if err != nil || cfg.Execution.Concurrency != 1 || cfg.Execution.Loop != 0 {
		t.Fatalf("default execution = %+v, %v", cfg.Execution, err)
	}
	for _, test := range []struct {
		execution string
		wantLoop  time.Duration
		wantErr   bool
	}{
		{`{"concurrency":5,"loop_duration":"250ms"}`, 250 * time.Millisecond, false},
		{`{"concurrency":1,"loop_duration":"1h30m"}`, 90 * time.Minute, false},
		{`{"concurrency":0}`, 0, true},
		{`{"concurrency":-1}`, 0, true},
		{`{"concurrency":1,"loop_duration":"0s"}`, 0, true},
		{`{"concurrency":1,"loop_duration":"bad"}`, 0, true},
		{`{"concurrency":1,"unknown":true}`, 0, true},
	} {
		content := strings.Replace(base, `"output_dir":"runs"`, `"execution":`+test.execution+`,"output_dir":"runs"`, 1)
		got, err := Decode(strings.NewReader(content))
		if (err != nil) != test.wantErr {
			t.Fatalf("execution %s error = %v", test.execution, err)
		}
		if err == nil && (got.Execution.Concurrency < 1 || got.Execution.Loop != test.wantLoop) {
			t.Fatalf("execution %s = %+v", test.execution, got.Execution)
		}
	}
}

func TestDecodeRequiresSupportedModelProvider(t *testing.T) {
	base := `{"name":"test","versions_file":"versions.json","benchmark":{"type":"terminalbench2","root":"root","tasks":["fix-git"]},"harness":{"type":"openclaw"},"sandbox":{"type":"docker"},"bridge":{"type":"openclaw-ssh"},"model":{"provider":"deepseek","base_url":"https://example.com","model":"model","api_key_env":"KEY"},"output_dir":"runs"}`
	for _, provider := range []string{"deepseek", "sglang"} {
		content := strings.Replace(base, `"provider":"deepseek"`, `"provider":"`+provider+`"`, 1)
		if provider == "sglang" {
			content = strings.Replace(content, "https://example.com", "https://example.com/v1", 1)
		}
		if _, err := Decode(strings.NewReader(content)); err != nil {
			t.Fatalf("provider %s: %v", provider, err)
		}
	}
	for _, content := range []string{strings.Replace(base, `"provider":"deepseek",`, "", 1), strings.Replace(base, `"provider":"deepseek"`, `"provider":"other"`, 1)} {
		if _, err := Decode(strings.NewReader(content)); err == nil {
			t.Fatalf("accepted provider document %s", content)
		}
	}
}

func TestDecodeSGLangBaseURLIsExactAndNormalized(t *testing.T) {
	base := `{"name":"test","versions_file":"versions.json","benchmark":{"type":"terminalbench2","root":"root","tasks":["fix-git"]},"harness":{"type":"openclaw"},"sandbox":{"type":"docker"},"bridge":{"type":"openclaw-ssh"},"model":{"provider":"sglang","base_url":"BASE","model":"model","api_key_env":"KEY"},"output_dir":"runs"}`
	valid := strings.Replace(base, "BASE", "https://host:30000/v1/", 1)
	cfg, err := Decode(strings.NewReader(valid))
	if err != nil || cfg.Model.BaseURL != "https://host:30000/v1" {
		t.Fatalf("normalized base URL = %q, %v", cfg.Model.BaseURL, err)
	}
	for _, invalid := range []string{"http://host/v1/v1", "http://host/v1?", "http://host/v%31"} {
		content := strings.Replace(base, "BASE", invalid, 1)
		if _, err := Decode(strings.NewReader(content)); err == nil {
			t.Fatalf("accepted SGLang base URL %q", invalid)
		}
	}
}

func TestLoadNonemptyOverridesFileFailuresIncludeReferencedPath(t *testing.T) {
	tests := []struct {
		name       string
		reference  string
		content    *string
		wantDetail string
	}{
		{name: "missing", reference: "../configs/missing.json", wantDetail: "open runtime overrides"},
		{name: "whitespace nonempty", reference: "   ", wantDetail: "open runtime overrides"},
		{name: "malformed", reference: "../configs/malformed.json", content: stringPointer(`{"harness_resources":`), wantDetail: "decode runtime overrides"},
		{name: "semantic", reference: "../configs/invalid.json", content: stringPointer(`{"agent_sandbox_resources":{"cpu":0}}`), wantDetail: "agent_sandbox_resources.cpu"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			profiles := filepath.Join(root, "profiles")
			configs := filepath.Join(root, "configs")
			if err := os.MkdirAll(profiles, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(configs, 0o755); err != nil {
				t.Fatal(err)
			}
			profile := strings.Replace(validConfig, `"versions_file": "../configs/versions.json",`, `"versions_file": "../configs/versions.json", "overrides_file": `+fmt.Sprintf("%q", test.reference)+`,`, 1)
			profilePath := filepath.Join(profiles, "experiment.json")
			if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configs, "versions.json"), []byte(validVersions), 0o600); err != nil {
				t.Fatal(err)
			}
			resolved := test.reference
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(profiles, resolved)
			}
			if test.content != nil {
				if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(resolved, []byte(*test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Load(profilePath)
			if err == nil || !strings.Contains(err.Error(), test.wantDetail) || !strings.Contains(err.Error(), resolved) {
				t.Fatalf("Load() error = %v, want detail %q and path %q", err, test.wantDetail, resolved)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }

const validConfig = `{
  "name": "test-run",
	"versions_file": "../configs/versions.json",
  "benchmark": {"type": "terminalbench2", "root": ".cache/tb2", "tasks": ["fix-git"]},
	"harness": {"type": "openclaw"},
  "sandbox": {"type": "docker"},
  "bridge": {"type": "openclaw-ssh"},
  "model": {"provider": "deepseek", "base_url": "http://127.0.0.1:8080", "model": "fake", "api_key_env": "DEEPSEEK_API_KEY"}
}`

const validVersions = `{
  "terminalbench2": {
    "repository_url": "https://example.invalid/terminal-bench-2.git",
	"revision": "0123456789abcdef0123456789abcdef01234567"
  },
  "openclaw": {
	"image": "example.invalid/openclaw:1.2.3@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  }
}`

func TestDecodeValidMinimalConfig(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.OutputDir != "runs" {
		t.Fatalf("OutputDir = %q, want runs", cfg.OutputDir)
	}
	if cfg.VersionsFile != "../configs/versions.json" || cfg.Benchmark.Tasks[0] != "fix-git" || cfg.Model.APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestLoadResolvesAndStrictlyLoadsVersionPins(t *testing.T) {
	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	configs := filepath.Join(root, "configs")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(profiles, "experiment.json")
	if err := os.WriteFile(profilePath, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configs, "versions.json"), []byte(validVersions), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Versions.TerminalBench2.Revision != "0123456789abcdef0123456789abcdef01234567" ||
		cfg.Versions.OpenClaw.Image == "" {
		t.Fatalf("version pins = %#v", cfg.Versions)
	}
}

func TestLoadEmptyOverridesFileDisablesOverrides(t *testing.T) {
	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	configs := filepath.Join(root, "configs")
	if err := os.MkdirAll(profiles, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := strings.Replace(validConfig, `"versions_file": "../configs/versions.json",`, `"versions_file": "../configs/versions.json", "overrides_file": "",`, 1)
	if err := os.WriteFile(filepath.Join(profiles, "experiment.json"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configs, "versions.json"), []byte(validVersions), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(profiles, "experiment.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OverridesFile != "" || !reflect.DeepEqual(cfg.Overrides, RuntimeOverrides{}) {
		t.Fatalf("overrides = %#v", cfg.Overrides)
	}
}

func TestCheckedInDeepSeekProfileLoads(t *testing.T) {
	path := filepath.Clean(filepath.Join("..", "..", "profiles", "openclaw-tb2-fix-git-deepseek.json"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "openclaw-tb2-fix-git-deepseek" || cfg.OverridesFile != "" || cfg.Versions.OpenClaw.Image == "" || cfg.Execution.Concurrency != 1 || cfg.Execution.Loop != 0 {
		t.Fatalf("checked-in profile = %#v", cfg)
	}
}

func TestCheckedInFiveTaskProfileLoadsInOrder(t *testing.T) {
	path := filepath.Clean(filepath.Join("..", "..", "profiles", "openclaw-tb2-five-deepseek.json"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"fix-git", "prove-plus-comm", "overfull-hbox", "rstan-to-pystan", "schemelike-metacircular-eval"}
	if strings.Join(cfg.Benchmark.Tasks, ",") != strings.Join(want, ",") || cfg.OverridesFile == "" || cfg.Execution.Concurrency != 5 || cfg.Execution.Loop != 0 {
		t.Fatalf("checked-in five-task profile = %#v", cfg)
	}
}

func TestCheckedInSGLangProfileLoadsWithoutEndpoint(t *testing.T) {
	path := filepath.Clean(filepath.Join("..", "..", "profiles", "openclaw-tb2-fix-git-sglang.json"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Provider != "sglang" || !strings.HasSuffix(cfg.Model.BaseURL, "/v1") || cfg.Model.APIKeyEnv != "SGLANG_API_KEY" {
		t.Fatalf("profile = %#v", cfg)
	}
}

func TestDecodeVersionsRejectsUnknownAndMissingFields(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"valid", validVersions, ""},
		{"unknown", strings.Replace(validVersions, `"image": "example.invalid/openclaw`, `"future": true, "image": "example.invalid/openclaw`, 1), `unknown field "future"`},
		{"missing revision", strings.Replace(validVersions, `"revision": "0123456789abcdef0123456789abcdef01234567"`, `"revision": ""`, 1), "terminalbench2.revision is required"},
		{"old task images rejected", strings.Replace(validVersions, `"revision": "0123456789abcdef0123456789abcdef01234567"`, `"revision": "0123456789abcdef0123456789abcdef01234567", "images": {}`, 1), `unknown field "images"`},
		{"mutable image", strings.Replace(validVersions, `example.invalid/openclaw:1.2.3@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`, `example.invalid/openclaw:latest`, 1), "openclaw.image: image must be pinned by digest"},
		{"malformed image", strings.Replace(validVersions, `example.invalid/openclaw:1.2.3@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`, `not a valid image@@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb`, 1), "invalid image reference"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeVersions(strings.NewReader(test.input))
			if test.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("DecodeVersions() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "unknown nested field",
			input:   strings.Replace(validConfig, `"type": "docker"`, `"type": "docker", "future": true`, 1),
			wantErr: `unknown field "future"`,
		},
		{
			name:    "secret value field",
			input:   strings.Replace(validConfig, `"api_key_env": "DEEPSEEK_API_KEY"`, `"api_key_env": "DEEPSEEK_API_KEY", "api_key": "must-not-persist"`, 1),
			wantErr: `unknown field "api_key"`,
		},
		{
			name:    "bad type",
			input:   strings.Replace(validConfig, `"tasks": ["fix-git"]`, `"tasks": "fix-git"`, 1),
			wantErr: "cannot unmarshal string",
		},
		{
			name:    "missing required field",
			input:   strings.Replace(validConfig, `"versions_file": "../configs/versions.json"`, `"versions_file": ""`, 1),
			wantErr: "versions_file is required",
		},
		{
			name:    "unsafe experiment name",
			input:   strings.Replace(validConfig, `"name": "test-run"`, `"name": "../escape"`, 1),
			wantErr: "name must contain only",
		},
		{
			name:    "overlong experiment name",
			input:   strings.Replace(validConfig, `"name": "test-run"`, `"name": "`+strings.Repeat("a", 81)+`"`, 1),
			wantErr: "name must not exceed 80 bytes",
		},
		{
			name:    "no task",
			input:   strings.Replace(validConfig, `["fix-git"]`, `[]`, 1),
			wantErr: "benchmark.tasks must contain",
		},
		{
			name:    "relative model URL",
			input:   strings.Replace(validConfig, `http://127.0.0.1:8080`, `/v1`, 1),
			wantErr: "absolute HTTP(S) URL",
		},
		{
			name:    "invalid environment name",
			input:   strings.Replace(validConfig, `DEEPSEEK_API_KEY`, `not-valid`, 1),
			wantErr: "environment variable name",
		},
		{
			name:    "trailing value",
			input:   validConfig + ` {}`,
			wantErr: "multiple JSON values",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(test.input))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Decode() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestDecodeAcceptsLowercaseAPIKeyEnvironmentName(t *testing.T) {
	input := strings.Replace(validConfig, "DEEPSEEK_API_KEY", "deepseek_api_key", 1)
	if _, err := Decode(strings.NewReader(input)); err != nil {
		t.Fatalf("Decode() rejected a valid environment name: %v", err)
	}
}
