package config

import (
	"strings"
	"testing"
)

const validConfig = `{
  "name": "test-run",
  "benchmark": {"type": "terminalbench2", "root": ".cache/tb2", "tasks": ["fix-git"]},
  "harness": {"type": "openclaw", "image": "example.invalid/openclaw@sha256:abc"},
  "sandbox": {"type": "docker"},
  "bridge": {"type": "openclaw-ssh"},
  "model": {"base_url": "http://127.0.0.1:8080", "model": "fake", "api_key_env": "DEEPSEEK_API_KEY"}
}`

func TestDecodeValidMinimalConfig(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if cfg.OutputDir != "runs" {
		t.Fatalf("OutputDir = %q, want runs", cfg.OutputDir)
	}
	if cfg.Benchmark.Tasks[0] != "fix-git" || cfg.Model.APIKeyEnv != "DEEPSEEK_API_KEY" {
		t.Fatalf("unexpected config: %#v", cfg)
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
			input:   strings.Replace(validConfig, `"image": "example.invalid/openclaw@sha256:abc"`, `"image": ""`, 1),
			wantErr: "harness.image is required",
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
