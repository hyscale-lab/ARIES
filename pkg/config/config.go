package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

const defaultOutputDir = "runs"

// Config is one explicit experiment file. It intentionally has no inheritance,
// profiles, templates, or secret values.
type Config struct {
	Name      string          `json:"name"`
	Benchmark BenchmarkConfig `json:"benchmark"`
	Harness   HarnessConfig   `json:"harness"`
	Sandbox   SandboxConfig   `json:"sandbox"`
	Bridge    BridgeConfig    `json:"bridge"`
	Model     ModelConfig     `json:"model"`
	OutputDir string          `json:"output_dir"`
}

type BenchmarkConfig struct {
	Type  string   `json:"type"`
	Root  string   `json:"root"`
	Tasks []string `json:"tasks"`
}

type HarnessConfig struct {
	Type  string `json:"type"`
	Image string `json:"image"`
}

type SandboxConfig struct {
	Type string `json:"type"`
}

type BridgeConfig struct {
	Type string `json:"type"`
}

type ModelConfig struct {
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

// Load reads and strictly validates one JSON experiment file.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open experiment config: %w", err)
	}
	defer f.Close()

	cfg, err := Decode(f)
	if err != nil {
		return Config{}, fmt.Errorf("decode experiment config %q: %w", path, err)
	}
	return cfg, nil
}

// Decode rejects unknown fields and trailing JSON values.
func Decode(r io.Reader) (Config, error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()

	var cfg Config
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("experiment config contains multiple JSON values")
		}
		return Config{}, fmt.Errorf("read trailing JSON: %w", err)
	}

	if cfg.OutputDir == "" {
		cfg.OutputDir = defaultOutputDir
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	checks := []struct {
		name  string
		value string
	}{
		{"name", c.Name},
		{"benchmark.type", c.Benchmark.Type},
		{"benchmark.root", c.Benchmark.Root},
		{"harness.type", c.Harness.Type},
		{"harness.image", c.Harness.Image},
		{"sandbox.type", c.Sandbox.Type},
		{"bridge.type", c.Bridge.Type},
		{"model.base_url", c.Model.BaseURL},
		{"model.model", c.Model.Model},
		{"model.api_key_env", c.Model.APIKeyEnv},
		{"output_dir", c.OutputDir},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return fmt.Errorf("%s is required", check.name)
		}
	}
	if len(c.Benchmark.Tasks) == 0 {
		return errors.New("benchmark.tasks must contain at least one task ID")
	}
	for i, task := range c.Benchmark.Tasks {
		if strings.TrimSpace(task) == "" {
			return fmt.Errorf("benchmark.tasks[%d] must not be empty", i)
		}
	}

	baseURL, err := url.Parse(c.Model.BaseURL)
	if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return fmt.Errorf("model.base_url must be an absolute HTTP(S) URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return errors.New("model.base_url must not contain credentials, a query, or a fragment")
	}
	if !validEnvName(c.Model.APIKeyEnv) {
		return errors.New("model.api_key_env must be an environment variable name")
	}
	return nil
}

func validEnvName(value string) bool {
	for i, r := range value {
		if r == '_' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}
