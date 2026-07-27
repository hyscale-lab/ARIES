package sglang

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// NativeConfig contains only launch-critical SGLang settings that ARIES
// validates before allowing setup or process side effects.
type NativeConfig struct {
	ModelPath          string  `yaml:"model-path"`
	ServedModelName    string  `yaml:"served-model-name"`
	Host               string  `yaml:"host"`
	Port               int     `yaml:"port"`
	Device             string  `yaml:"device"`
	TensorParallelSize int     `yaml:"tensor-parallel-size"`
	ContextLength      int     `yaml:"context-length"`
	MemFractionStatic  float64 `yaml:"mem-fraction-static"`
	ReasoningParser    string  `yaml:"reasoning-parser"`
	ToolCallParser     string  `yaml:"tool-call-parser"`
}

func LoadNativeConfig(path, modelID, baseURL string) (NativeConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return NativeConfig{}, fmt.Errorf("open SGLang config: %w", err)
	}
	defer f.Close()
	decoder := yaml.NewDecoder(f)
	decoder.KnownFields(true)
	var cfg NativeConfig
	if err := decoder.Decode(&cfg); err != nil {
		return NativeConfig{}, fmt.Errorf("decode SGLang config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return NativeConfig{}, errors.New("decode SGLang config: multiple YAML documents")
		}
		return NativeConfig{}, fmt.Errorf("decode SGLang config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return NativeConfig{}, fmt.Errorf("validate SGLang config: %w", err)
	}
	if cfg.ServedModelName != modelID {
		return NativeConfig{}, errors.New("served-model-name must equal model.id")
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Port() == "" {
		return NativeConfig{}, errors.New("model.base_url must contain an explicit port")
	}
	port, err := strconv.Atoi(endpoint.Port())
	if err != nil || port != cfg.Port {
		return NativeConfig{}, errors.New("port must equal the explicit port in model.base_url")
	}
	return cfg, nil
}

func (c NativeConfig) validate() error {
	required := []struct{ name, value string }{
		{"model-path", c.ModelPath}, {"served-model-name", c.ServedModelName},
		{"host", c.Host}, {"device", c.Device}, {"tool-call-parser", c.ToolCallParser},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if c.TensorParallelSize <= 0 {
		return errors.New("tensor-parallel-size must be positive")
	}
	if c.ContextLength <= 0 {
		return errors.New("context-length must be positive")
	}
	if c.MemFractionStatic <= 0 || c.MemFractionStatic > 1 || math.IsNaN(c.MemFractionStatic) || math.IsInf(c.MemFractionStatic, 0) {
		return errors.New("mem-fraction-static must be finite, positive, and no greater than 1")
	}
	return nil
}
