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
	"github.com/hyscale-lab/aries/pkg/core"
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
	Runtime       RuntimeConfig    `json:"runtime"`
	Model         ProfileModel     `json:"model"`
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

type RuntimeConfig struct {
	Backend string              `json:"backend"`
	Mode    string              `json:"mode"`
	Config  RuntimeConfigValues `json:"config,omitempty"`
}

type RuntimeConfigValues struct {
	File               string        `json:"file,omitempty"`
	Executable         string        `json:"executable,omitempty"`
	StartupTimeoutText string        `json:"startup_timeout,omitempty"`
	StopTimeoutText    string        `json:"stop_timeout,omitempty"`
	StartupTimeout     time.Duration `json:"-"`
	StopTimeout        time.Duration `json:"-"`
	ResolvedFile       string        `json:"-"`
	GPUIndices         []int         `json:"gpu_indices,omitempty"`
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

type ProfileModel struct {
	ID        string `json:"id"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
}

type BenchmarkConfig struct {
	Type  string   `json:"type"`
	Root  string   `json:"root"`
	Tasks []string `json:"tasks"`
}

type HarnessConfig struct {
	Type     string                `json:"type"`
	Mode     string                `json:"mode,omitempty"`
	Realtime HarnessRealtimeConfig `json:"realtime,omitempty"`
}

type HarnessRealtimeConfig struct {
	AgentQuestionTemplate string            `json:"agent_question_template,omitempty"`
	TTS                   RealtimeTTSConfig `json:"tts,omitempty"`
	ChunkDurationText     string            `json:"chunk_duration,omitempty"`
	ListenDurationText    string            `json:"listen_duration,omitempty"`
	QuietDurationText     string            `json:"quiet_duration,omitempty"`
	AgentWaitDurationText string            `json:"agent_wait_duration,omitempty"`
	ToolCallTimeoutText   string            `json:"tool_call_timeout,omitempty"`
	TrailingSilenceMillis int               `json:"trailing_silence_ms,omitempty"`
	Provider              string            `json:"provider,omitempty"`
	Model                 string            `json:"model,omitempty"`
	Voice                 string            `json:"voice,omitempty"`
	ReasoningEffort       string            `json:"reasoning_effort,omitempty"`
	IncludeEvents         bool              `json:"include_events,omitempty"`
	ChunkDuration         time.Duration     `json:"-"`
	ListenDuration        time.Duration     `json:"-"`
	QuietDuration         time.Duration     `json:"-"`
	AgentWaitDuration     time.Duration     `json:"-"`
	ToolCallTimeout       time.Duration     `json:"-"`
}

type RealtimeTTSConfig struct {
	Provider     string        `json:"provider,omitempty"`
	BaseURL      string        `json:"base_url,omitempty"`
	APIKeyEnv    string        `json:"api_key_env,omitempty"`
	Model        string        `json:"model,omitempty"`
	Voice        string        `json:"voice,omitempty"`
	Instructions string        `json:"instructions,omitempty"`
	Speed        *float64      `json:"speed,omitempty"`
	TimeoutText  string        `json:"timeout,omitempty"`
	Timeout      time.Duration `json:"-"`
}

type SandboxConfig struct {
	Type string `json:"type"`
}

type BridgeConfig struct {
	Type string `json:"type"`
	// RetainRawLog keeps the bridge's byte-level ssh_raw.log. It defaults to
	// true because that log is the only record of raw wire commands, request
	// payloads, and binary stdin; profiles that only need the structured
	// tool-call log can set it false and drop the largest artifact a run
	// writes.
	RetainRawLog *bool `json:"retain_raw_log,omitempty"`
}

// RetainBridgeRawLog reports whether ssh_raw.log should be written, defaulting
// to true when a profile does not mention it.
func (c BridgeConfig) RetainBridgeRawLog() bool {
	return c.RetainRawLog == nil || *c.RetainRawLog
}

func (c Config) CoreModel() core.ModelConfig {
	return core.ModelConfig{Provider: c.Runtime.Backend, BaseURL: c.Model.BaseURL, Model: c.Model.ID, APIKeyEnv: c.Model.APIKeyEnv}
}

// Versions contains the upstream version selections shared by profiles.
type Versions struct {
	TerminalBench2 TerminalBench2Versions `json:"terminalbench2"`
	OpenClaw       OpenClawVersions       `json:"openclaw"`
	Hermes         HermesVersions         `json:"hermes"`
}

type TerminalBench2Versions struct {
	RepositoryURL string `json:"repository_url"`
	Revision      string `json:"revision"`
}

type OpenClawVersions struct {
	Image string `json:"image"`
}

type HermesVersions struct {
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
	if cfg.Runtime.Config.File != "" {
		resolved := cfg.Runtime.Config.File
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(path), resolved)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return Config{}, fmt.Errorf("resolve runtime config file: %w", err)
		}
		cfg.Runtime.Config.ResolvedFile = resolved
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

// DecodeVersions rejects unknown fields and invalid version selections.
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
		{"runtime.backend", c.Runtime.Backend},
		{"runtime.mode", c.Runtime.Mode},
		{"model.base_url", c.Model.BaseURL},
		{"model.id", c.Model.ID},
		{"model.api_key_env", c.Model.APIKeyEnv},
		{"output_dir", c.OutputDir},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.value) == "" {
			return fmt.Errorf("%s is required", check.name)
		}
	}
	if c.Runtime.Backend != "deepseek" && c.Runtime.Backend != "sglang" {
		return errors.New("runtime.backend must be deepseek or sglang")
	}
	if c.Harness.Mode == "" {
		c.Harness.Mode = "agent"
	}
	if err := c.Harness.validate(); err != nil {
		return err
	}
	if err := c.Runtime.validate(); err != nil {
		return err
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

	if c.Runtime.Backend == "sglang" {
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

func (h *HarnessConfig) validate() error {
	if h.Mode == "" {
		h.Mode = "agent"
	}
	switch h.Mode {
	case "agent":
		if h.Realtime != (HarnessRealtimeConfig{}) {
			return errors.New("harness.realtime must be empty unless harness.mode is realtime")
		}
		return nil
	case "realtime":
		if h.Type != "openclaw" {
			return errors.New("harness.mode realtime requires OpenClaw")
		}
		if err := h.Realtime.TTS.validate(); err != nil {
			return err
		}
		if h.Realtime.TrailingSilenceMillis < 0 {
			return errors.New("harness.realtime.trailing_silence_ms must not be negative")
		}
		var err error
		if h.Realtime.ChunkDuration, err = parseOptionalPositiveDuration("harness.realtime.chunk_duration", h.Realtime.ChunkDurationText); err != nil {
			return err
		}
		if h.Realtime.ListenDuration, err = parseOptionalPositiveDuration("harness.realtime.listen_duration", h.Realtime.ListenDurationText); err != nil {
			return err
		}
		if h.Realtime.QuietDuration, err = parseOptionalPositiveDuration("harness.realtime.quiet_duration", h.Realtime.QuietDurationText); err != nil {
			return err
		}
		if h.Realtime.AgentWaitDuration, err = parseOptionalPositiveDuration("harness.realtime.agent_wait_duration", h.Realtime.AgentWaitDurationText); err != nil {
			return err
		}
		if h.Realtime.ToolCallTimeout, err = parseOptionalPositiveDuration("harness.realtime.tool_call_timeout", h.Realtime.ToolCallTimeoutText); err != nil {
			return err
		}
		return nil
	default:
		return errors.New("harness.mode must be agent or realtime")
	}
}

func (tts *RealtimeTTSConfig) validate() error {
	if tts.Provider == "" {
		tts.Provider = "openai"
	}
	if tts.Provider != "openai" {
		return errors.New("harness.realtime.tts.provider must be openai")
	}
	if tts.APIKeyEnv == "" {
		tts.APIKeyEnv = "OPENAI_API_KEY"
	}
	if !validEnvName(tts.APIKeyEnv) {
		return errors.New("harness.realtime.tts.api_key_env must be an environment variable name")
	}
	if strings.ContainsRune(tts.BaseURL, 0) || strings.ContainsRune(tts.Model, 0) || strings.ContainsRune(tts.Voice, 0) || strings.ContainsRune(tts.Instructions, 0) {
		return errors.New("harness.realtime.tts contains an invalid NUL byte")
	}
	if tts.Speed != nil && (*tts.Speed < 0.25 || *tts.Speed > 4 || math.IsNaN(*tts.Speed) || math.IsInf(*tts.Speed, 0)) {
		return errors.New("harness.realtime.tts.speed must be between 0.25 and 4")
	}
	var err error
	tts.Timeout, err = parseOptionalPositiveDuration("harness.realtime.tts.timeout", tts.TimeoutText)
	return err
}

func parseOptionalPositiveDuration(name, value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", name)
	}
	return duration, nil
}

func (c *RuntimeConfig) validate() error {
	switch c.Backend {
	case "deepseek":
		if c.Mode != "external" {
			return errors.New("runtime.backend deepseek requires external mode")
		}
		if c.Config.File != "" || c.Config.Executable != "" || c.Config.StartupTimeoutText != "" || c.Config.StopTimeoutText != "" || len(c.Config.GPUIndices) != 0 {
			return errors.New("external deepseek runtime.config must be empty")
		}
		return nil
	case "sglang":
	default:
		return errors.New("runtime.backend must be deepseek or sglang")
	}
	switch c.Mode {
	case "external":
		if strings.TrimSpace(c.Config.File) == "" {
			return errors.New("external SGLang runtime.config.file is required")
		}
		if c.Config.Executable != "" || c.Config.StartupTimeoutText != "" || c.Config.StopTimeoutText != "" || len(c.Config.GPUIndices) != 0 {
			return errors.New("external SGLang runtime.config must not set executable, timeouts, or gpu_indices")
		}
		return nil
	case "managed":
		if strings.TrimSpace(c.Config.File) == "" {
			return errors.New("managed SGLang runtime.config.file is required")
		}
		if strings.TrimSpace(c.Config.Executable) == "" || strings.ContainsRune(c.Config.Executable, 0) {
			return errors.New("managed SGLang runtime.config.executable is required")
		}
		seenGPU := make(map[int]struct{}, len(c.Config.GPUIndices))
		for _, index := range c.Config.GPUIndices {
			if index < 0 {
				return errors.New("managed SGLang runtime.config.gpu_indices must contain only non-negative indices")
			}
			if _, exists := seenGPU[index]; exists {
				return errors.New("managed SGLang runtime.config.gpu_indices must not contain duplicates")
			}
			seenGPU[index] = struct{}{}
		}
		var err error
		c.Config.StartupTimeout, err = time.ParseDuration(c.Config.StartupTimeoutText)
		if err != nil || c.Config.StartupTimeout <= 0 {
			return errors.New("managed SGLang runtime.config.startup_timeout must be a positive Go duration")
		}
		c.Config.StopTimeout, err = time.ParseDuration(c.Config.StopTimeoutText)
		if err != nil || c.Config.StopTimeout <= 0 {
			return errors.New("managed SGLang runtime.config.stop_timeout must be a positive Go duration")
		}
		return nil
	default:
		return errors.New("runtime.mode must be external or managed")
	}
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
	if err := containerimage.ValidatePinnedTagOnly(c.OpenClaw.Image); err != nil {
		return fmt.Errorf("openclaw.image: %w", err)
	}
	// A catalog predating a given harness stays valid: only the image the
	// selected harness actually needs is required, and that is enforced by
	// HarnessImage once the profile's harness type is known. Any image that is
	// present must still be pinned.
	if strings.TrimSpace(c.Hermes.Image) != "" {
		if err := containerimage.ValidatePinnedTagOnly(c.Hermes.Image); err != nil {
			return fmt.Errorf("hermes.image: %w", err)
		}
	}
	return nil
}

// HarnessImage returns the pinned image the named harness runs from. The set of
// harnesses is enumerated rather than discovered, so an unknown type is an
// error instead of an empty result.
func (c Versions) HarnessImage(harnessType string) (string, error) {
	var image, field string
	switch harnessType {
	case "openclaw":
		image, field = c.OpenClaw.Image, "openclaw.image"
	case "hermes":
		image, field = c.Hermes.Image, "hermes.image"
	default:
		return "", fmt.Errorf("unsupported harness type %q", harnessType)
	}
	if strings.TrimSpace(image) == "" {
		return "", fmt.Errorf("%s is required for harness type %q", field, harnessType)
	}
	return image, nil
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
