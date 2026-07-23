package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `{
  "name": "test-run",
	"versions_file": "../configs/versions.json",
  "benchmark": {"type": "terminalbench2", "root": ".cache/tb2", "tasks": ["fix-git"]},
	"harness": {"type": "openclaw"},
  "sandbox": {"type": "docker"},
  "bridge": {"type": "openclaw-ssh"},
  "model": {"base_url": "http://127.0.0.1:8080", "model": "fake", "api_key_env": "DEEPSEEK_API_KEY"}
}`

const validVersions = `{
  "terminalbench2": {
    "repository_url": "https://example.invalid/terminal-bench-2.git",
    "revision": "0123456789abcdef0123456789abcdef01234567",
	"images": {
	  "example.invalid/fix-git:fixture": "example.invalid/fix-git:fixture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
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
		len(cfg.Versions.TerminalBench2.Images) != 1 || cfg.Versions.OpenClaw.Image == "" {
		t.Fatalf("version pins = %#v", cfg.Versions)
	}
}

func TestCheckedInDeepSeekProfileLoads(t *testing.T) {
	path := filepath.Clean(filepath.Join("..", "..", "profiles", "openclaw-tb2-fix-git-deepseek.json"))
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "openclaw-tb2-fix-git-deepseek" || len(cfg.Versions.TerminalBench2.Images) != 89 || cfg.Versions.OpenClaw.Image == "" {
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
	if strings.Join(cfg.Benchmark.Tasks, ",") != strings.Join(want, ",") || len(cfg.Versions.TerminalBench2.Images) != 89 {
		t.Fatalf("checked-in five-task profile = %#v", cfg)
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
		{"missing task images", strings.Replace(validVersions, `"example.invalid/fix-git:fixture": "example.invalid/fix-git:fixture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, ``, 1), "terminalbench2.images must contain"},
		{"mutable task image", strings.Replace(validVersions, `example.invalid/fix-git:fixture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, `example.invalid/fix-git:latest`, 1), "terminalbench2.images"},
		{"mismatched task image", strings.Replace(validVersions, `example.invalid/fix-git:fixture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, `example.invalid/other:fixture@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`, 1), "does not match source"},
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
