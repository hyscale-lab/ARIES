package config

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/containerimage"
)

const defaultOutputDir = "runs"

// Config is one explicit experiment profile. It has no inheritance, templates,
// merging, or secret values.
type Config struct {
	Name         string          `json:"name"`
	VersionsFile string          `json:"versions_file"`
	Benchmark    BenchmarkConfig `json:"benchmark"`
	Harness      HarnessConfig   `json:"harness"`
	Sandbox      SandboxConfig   `json:"sandbox"`
	Bridge       BridgeConfig    `json:"bridge"`
	Model        ModelConfig     `json:"model"`
	OutputDir    string          `json:"output_dir"`
	Versions     Versions        `json:"-"`
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
	FixGitImage   string `json:"fix_git_image"`
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
	return cfg, nil
}

// Decode rejects unknown fields and trailing JSON values.
func Decode(r io.Reader) (Config, error) {
	var cfg Config
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

func (c Config) validate() error {
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

func (c Versions) validate() error {
	checks := []struct {
		name  string
		value string
	}{
		{"terminalbench2.repository_url", c.TerminalBench2.RepositoryURL},
		{"terminalbench2.revision", c.TerminalBench2.Revision},
		{"terminalbench2.fix_git_image", c.TerminalBench2.FixGitImage},
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
	for _, image := range []struct {
		name  string
		value string
	}{
		{"terminalbench2.fix_git_image", c.TerminalBench2.FixGitImage},
		{"openclaw.image", c.OpenClaw.Image},
	} {
		if err := containerimage.Validate(image.value); err != nil {
			return fmt.Errorf("%s: %w", image.name, err)
		}
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
		if r == '_' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}
