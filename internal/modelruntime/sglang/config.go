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
	ModelPath                    string  `yaml:"model-path"`
	ServedModelName              string  `yaml:"served-model-name"`
	Host                         string  `yaml:"host"`
	Port                         int     `yaml:"port"`
	Device                       string  `yaml:"device"`
	TensorParallelSize           int     `yaml:"tensor-parallel-size"`
	PipelineParallelSize         *int    `yaml:"pipeline-parallel-size"`
	DataParallelSize             *int    `yaml:"data-parallel-size"`
	EnableDPAttention            bool    `yaml:"enable-dp-attention"`
	ExpertParallelSize           *int    `yaml:"expert-parallel-size"`
	AttentionContextParallelSize *int    `yaml:"attention-context-parallel-size"`
	MoEDataParallelSize          *int    `yaml:"moe-data-parallel-size"`
	Nodes                        *int    `yaml:"nnodes"`
	NodeRank                     *int    `yaml:"node-rank"`
	ContextLength                int     `yaml:"context-length"`
	MemFractionStatic            float64 `yaml:"mem-fraction-static"`
	ReasoningParser              string  `yaml:"reasoning-parser"`
	ToolCallParser               string  `yaml:"tool-call-parser"`
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
	for name, value := range map[string]*int{
		"pipeline-parallel-size":          c.PipelineParallelSize,
		"data-parallel-size":              c.DataParallelSize,
		"expert-parallel-size":            c.ExpertParallelSize,
		"attention-context-parallel-size": c.AttentionContextParallelSize,
		"moe-data-parallel-size":          c.MoEDataParallelSize,
		"nnodes":                          c.Nodes,
	} {
		if value != nil && *value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	nodes := valueOrOne(c.Nodes)
	nodeRank := 0
	if c.NodeRank != nil {
		nodeRank = *c.NodeRank
	}
	if nodeRank < 0 || nodeRank >= nodes {
		return errors.New("node-rank must be non-negative and less than nnodes")
	}
	if c.ContextLength <= 0 {
		return errors.New("context-length must be positive")
	}
	if c.MemFractionStatic <= 0 || c.MemFractionStatic > 1 || math.IsNaN(c.MemFractionStatic) || math.IsInf(c.MemFractionStatic, 0) {
		return errors.New("mem-fraction-static must be finite, positive, and no greater than 1")
	}
	return nil
}

func (c NativeConfig) GPUCount() (int, error) {
	tensor := c.TensorParallelSize
	pipeline := valueOrOne(c.PipelineParallelSize)
	data := valueOrOne(c.DataParallelSize)
	attentionData := 1
	if c.EnableDPAttention {
		attentionData = data
	}
	attentionContext := valueOrOne(c.AttentionContextParallelSize)
	if tensor%attentionData != 0 || tensor/attentionData%attentionContext != 0 {
		return 0, errors.New("tensor-parallel-size must be divisible by data-parallel-size and attention-context-parallel-size when DP attention is enabled")
	}
	moeData := valueOrOne(c.MoEDataParallelSize)
	expert := valueOrOne(c.ExpertParallelSize)
	if tensor%moeData != 0 || tensor/moeData%expert != 0 {
		return 0, errors.New("tensor-parallel-size must be divisible by moe-data-parallel-size and expert-parallel-size")
	}
	nodes := valueOrOne(c.Nodes)
	var localPipeline, localTensor int
	if pipeline >= nodes {
		if pipeline%nodes != 0 {
			return 0, errors.New("pipeline-parallel-size must be divisible by nnodes when it is at least nnodes")
		}
		localPipeline, localTensor = pipeline/nodes, tensor
	} else {
		if nodes%pipeline != 0 {
			return 0, errors.New("nnodes must be divisible by pipeline-parallel-size when it exceeds pipeline-parallel-size")
		}
		nodesPerPipelineRank := nodes / pipeline
		if tensor%nodesPerPipelineRank != 0 {
			return 0, errors.New("tensor-parallel-size must be divisible by nodes per pipeline rank")
		}
		localPipeline, localTensor = 1, tensor/nodesPerPipelineRank
	}
	replicas := data
	if c.EnableDPAttention {
		replicas = 1
	}
	if localPipeline > int(^uint(0)>>1)/localTensor || localPipeline*localTensor > int(^uint(0)>>1)/replicas {
		return 0, errors.New("parallel configuration requires too many GPUs")
	}
	return localPipeline * localTensor * replicas, nil
}

func (c NativeConfig) ResolveGPUIndices(configured []int) ([]int, error) {
	if c.Device != "cuda" {
		if len(configured) != 0 {
			return nil, errors.New("gpu_indices requires SGLang device cuda")
		}
		return nil, nil
	}
	required, err := c.GPUCount()
	if err != nil {
		return nil, err
	}
	if len(configured) == 0 {
		indices := make([]int, required)
		for index := range indices {
			indices[index] = index
		}
		return indices, nil
	}
	if len(configured) != required {
		return nil, fmt.Errorf("gpu_indices has %d GPUs, but SGLang config requires %d", len(configured), required)
	}
	seen := make(map[int]struct{}, len(configured))
	for _, index := range configured {
		if index < 0 {
			return nil, errors.New("gpu_indices must contain only non-negative indices")
		}
		if _, exists := seen[index]; exists {
			return nil, errors.New("gpu_indices must not contain duplicates")
		}
		seen[index] = struct{}{}
	}
	return append([]int(nil), configured...), nil
}

func valueOrOne(value *int) int {
	if value == nil {
		return 1
	}
	return *value
}
