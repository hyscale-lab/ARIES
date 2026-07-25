package config

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

const defaultOutputDir = "runs"

// Config is one explicit experiment profile. It has no inheritance, templates,
// merging, or secret values.
type Config struct {
	Name          string           `json:"name"`
	VersionsFile  string           `json:"versions_file"`
	OverridesFile string           `json:"overrides_file,omitempty"`
	Benchmark     BenchmarkConfig  `json:"benchmark"`
	Harness       HarnessConfig    `json:"harness"`
	Sandbox       SandboxConfig    `json:"sandbox"`
	Bridge        BridgeConfig     `json:"bridge"`
	Model         ModelConfig      `json:"model"`
	Execution     ExecutionConfig  `json:"execution,omitempty"`
	OutputDir     string           `json:"output_dir"`
	Versions      Versions         `json:"-"`
	Overrides     RuntimeOverrides `json:"-"`
}

// ExecutionConfig controls bounded occurrence scheduling above the Runner.
type ExecutionConfig struct {
	Concurrency  int           `json:"concurrency"`
	LoopDuration string        `json:"loop_duration,omitempty"`
	Loop         time.Duration `json:"-"`
}

// RuntimeOverrides contains sparse, explicitly present runtime changes.
type RuntimeOverrides struct {
	HarnessResources      ResourceOverrides `json:"harness_resources,omitempty"`
	AgentSandboxResources ResourceOverrides `json:"agent_sandbox_resources,omitempty"`
	AgentTimeoutSeconds   *float64          `json:"agent_timeout_seconds,omitempty"`
	AgentTimeout          *time.Duration    `json:"-"`
}

// ResourceOverrides changes only the named container resource dimensions.
type ResourceOverrides struct {
	CPU      *float64 `json:"cpu,omitempty"`
	MemoryMB *int     `json:"memory_mb,omitempty"`
}

type BenchmarkConfig struct {
	Type  string   `json:"type"`
	Root  string   `json:"root"`
	Tasks []string `json:"tasks"`
}

type HarnessConfig struct {
	Type string `json:"type"`
}

type SandboxConfig struct {
	Type string `json:"type"`
}

type BridgeConfig struct {
	Type string `json:"type"`
}

type ModelConfig struct {
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

// Versions contains the immutable upstream identities shared by profiles.
type Versions struct {
	TerminalBench2 TerminalBench2Versions `json:"terminalbench2"`
	OpenClaw       OpenClawVersions       `json:"openclaw"`
}

type TerminalBench2Versions struct {
	RepositoryURL string `json:"repository_url"`
	Revision      string `json:"revision"`
}

type OpenClawVersions struct {
	Image string `json:"image"`
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
	versionsPath := cfg.VersionsFile
	if !filepath.IsAbs(versionsPath) {
		versionsPath = filepath.Join(filepath.Dir(path), versionsPath)
	}
	versions, err := LoadVersions(versionsPath)
	if err != nil {
		return Config{}, fmt.Errorf("load version pins: %w", err)
	}
	cfg.Versions = versions
	if cfg.OverridesFile != "" {
		overridesPath := cfg.OverridesFile
		if !filepath.IsAbs(overridesPath) {
			overridesPath = filepath.Join(filepath.Dir(path), overridesPath)
		}
		overrides, err := LoadRuntimeOverrides(overridesPath)
		if err != nil {
			return Config{}, fmt.Errorf("load runtime overrides: %w", err)
		}
		cfg.Overrides = overrides
	}
	return cfg, nil
}

// LoadRuntimeOverrides reads one dedicated strict runtime override document.
func LoadRuntimeOverrides(path string) (RuntimeOverrides, error) {
	f, err := os.Open(path)
	if err != nil {
		return RuntimeOverrides{}, fmt.Errorf("open runtime overrides %q: %w", path, err)
	}
	defer f.Close()
	var overrides RuntimeOverrides
	if err := decodeStrictJSON(f, &overrides, "runtime overrides"); err != nil {
		return RuntimeOverrides{}, fmt.Errorf("decode runtime overrides %q: %w", path, err)
	}
	if err := overrides.validate(); err != nil {
		return RuntimeOverrides{}, fmt.Errorf("validate runtime overrides %q: %w", path, err)
	}
	return overrides, nil
}

func (o *RuntimeOverrides) validate() error {
	if err := validateResources("harness_resources", o.HarnessResources); err != nil {
		return err
	}
	if err := validateResources("agent_sandbox_resources", o.AgentSandboxResources); err != nil {
		return err
	}
	if o.AgentTimeoutSeconds != nil {
		scaled := *o.AgentTimeoutSeconds * float64(time.Second)
		if *o.AgentTimeoutSeconds <= 0 || math.IsNaN(*o.AgentTimeoutSeconds) || math.IsInf(*o.AgentTimeoutSeconds, 0) || scaled >= math.Exp2(63) {
			return errors.New("agent_timeout_seconds must be finite, positive, and convert to nanoseconds below 2^63")
		}
		duration := time.Duration(scaled)
		o.AgentTimeout = &duration
	}
	return nil
}

func validateResources(name string, resources ResourceOverrides) error {
	if resources.CPU != nil {
		scaled := *resources.CPU * 1e9
		if *resources.CPU <= 0 || math.IsNaN(*resources.CPU) || math.IsInf(*resources.CPU, 0) || scaled >= math.Exp2(63) {
			return fmt.Errorf("%s.cpu must be finite, positive, and convert to NanoCPUs below 2^63", name)
		}
	}
	if resources.MemoryMB != nil && (*resources.MemoryMB <= 0 || int64(*resources.MemoryMB) > math.MaxInt64>>20) {
		return fmt.Errorf("%s.memory_mb must be positive and no greater than %d", name, int64(math.MaxInt64)>>20)
	}
	return nil
}

// Decode rejects unknown fields and trailing JSON values.
func Decode(r io.Reader) (Config, error) {
	cfg := Config{Execution: ExecutionConfig{Concurrency: 1}}
	if err := decodeStrictJSON(r, &cfg, "experiment config"); err != nil {
		return Config{}, err
	}

	if cfg.OutputDir == "" {
		cfg.OutputDir = defaultOutputDir
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadVersions reads one strict version pin catalog.
func LoadVersions(path string) (Versions, error) {
	f, err := os.Open(path)
	if err != nil {
		return Versions{}, fmt.Errorf("open version pins %q: %w", path, err)
	}
	defer f.Close()
	versions, err := DecodeVersions(f)
	if err != nil {
		return Versions{}, fmt.Errorf("decode version pins %q: %w", path, err)
	}
	return versions, nil
}

// DecodeVersions rejects unknown fields and mutable image references.
func DecodeVersions(r io.Reader) (Versions, error) {
	var versions Versions
	if err := decodeStrictJSON(r, &versions, "version pins"); err != nil {
		return Versions{}, err
	}
	if err := versions.validate(); err != nil {
		return Versions{}, err
	}
	return versions, nil
}

func decodeStrictJSON(r io.Reader, destination any, name string) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s contains multiple JSON values", name)
		}
		return fmt.Errorf("read trailing JSON: %w", err)
	}
	return nil
}

func (c *Config) validate() error {
	if c.Execution.Concurrency <= 0 {
		return errors.New("execution.concurrency must be positive")
	}
	if c.Execution.LoopDuration != "" {
		loop, err := time.ParseDuration(c.Execution.LoopDuration)
		if err != nil || loop <= 0 {
			return errors.New("execution.loop_duration must be a positive Go duration")
		}
		c.Execution.Loop = loop
	}
	checks := []struct {
		name  string
		value string
	}{
		{"name", c.Name},
		{"versions_file", c.VersionsFile},
		{"benchmark.type", c.Benchmark.Type},
		{"benchmark.root", c.Benchmark.Root},
		{"harness.type", c.Harness.Type},
		{"sandbox.type", c.Sandbox.Type},
		{"bridge.type", c.Bridge.Type},
		{"model.provider", c.Model.Provider},
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
	if c.Model.Provider != "deepseek" && c.Model.Provider != "sglang" {
		return errors.New("model.provider must be deepseek or sglang")
	}
	if err := validateExperimentName(c.Name); err != nil {
		return err
	}
	if len(c.Benchmark.Tasks) == 0 {
		return errors.New("benchmark.tasks must contain at least one task ID")
	}
	for i, task := range c.Benchmark.Tasks {
		if strings.TrimSpace(task) == "" {
			return fmt.Errorf("benchmark.tasks[%d] must not be empty", i)
		}
	}

	if c.Model.Provider == "sglang" {
		normalized, err := normalizeSGLangBaseURL(c.Model.BaseURL)
		if err != nil {
			return fmt.Errorf("model.base_url for sglang: %w", err)
		}
		c.Model.BaseURL = normalized
	} else {
		baseURL, err := url.Parse(c.Model.BaseURL)
		if err != nil || (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
			return fmt.Errorf("model.base_url must be an absolute HTTP(S) URL")
		}
		if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
			return errors.New("model.base_url must not contain credentials, a query, or a fragment")
		}
	}
	if !validEnvName(c.Model.APIKeyEnv) {
		return errors.New("model.api_key_env must be an environment variable name")
	}
	return nil
}

func normalizeSGLangBaseURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(baseURL, "#") {
		return "", errors.New("must be an absolute HTTP(S) URL without credentials, escaped path, query, or fragment")
	}
	if parsed.Path != "/v1" && parsed.Path != "/v1/" {
		return "", errors.New("path must be exactly /v1")
	}
	parsed.Path = "/v1"
	return parsed.String(), nil
}

func validateExperimentName(name string) error {
	if len(name) > 80 {
		return errors.New("name must not exceed 80 bytes")
	}
	for index, character := range name {
		allowed := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.'
		if !allowed || index == 0 && (character == '-' || character == '.') {
			return errors.New("name must contain only ASCII letters, digits, dashes, underscores, or dots and must not begin with a dash or dot")
		}
	}
	return nil
}

func (c Versions) validate() error {
	checks := []struct {
		name  string
		value string
	}{
		{"terminalbench2.repository_url", c.TerminalBench2.RepositoryURL},
		{"terminalbench2.revision", c.TerminalBench2.Revision},
		{"openclaw.image", c.OpenClaw.Image},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return fmt.Errorf("%s is required", check.name)
		}
	}
	repository, err := url.Parse(c.TerminalBench2.RepositoryURL)
	if err != nil || repository.Scheme != "https" || repository.Host == "" {
		return errors.New("terminalbench2.repository_url must be an absolute HTTPS URL")
	}
	if repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" {
		return errors.New("terminalbench2.repository_url must not contain credentials, a query, or a fragment")
	}
	if !isHex(c.TerminalBench2.Revision, 40) {
		return errors.New("terminalbench2.revision must be a 40-character Git revision")
	}
	if err := containerimage.Validate(c.OpenClaw.Image); err != nil {
		return fmt.Errorf("openclaw.image: %w", err)
	}
	return nil
}

func isHex(value string, characters int) bool {
	if len(value) != characters {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validEnvName(value string) bool {
	for i, r := range value {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}
