package sglang

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// NativeConfig contains the SGLang settings ARIES needs for endpoint routing
// and GPU measurement. SGLang validates the rest of the original config.
type NativeConfig struct {
	ServedModelName              string `yaml:"served-model-name"`
	Host                         string `yaml:"host"`
	Port                         int    `yaml:"port"`
	Device                       string `yaml:"device"`
	TensorParallelSize           int    `yaml:"tensor-parallel-size"`
	PipelineParallelSize         *int   `yaml:"pipeline-parallel-size"`
	DataParallelSize             *int   `yaml:"data-parallel-size"`
	EnableDPAttention            bool   `yaml:"enable-dp-attention"`
	ExpertParallelSize           *int   `yaml:"expert-parallel-size"`
	AttentionContextParallelSize *int   `yaml:"attention-context-parallel-size"`
	MoEDataParallelSize          *int   `yaml:"moe-data-parallel-size"`
	Nodes                        *int   `yaml:"nnodes"`
	NodeRank                     *int   `yaml:"node-rank"`
}

func LoadNativeConfig(path, modelID, baseURL string) (NativeConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return NativeConfig{}, fmt.Errorf("open SGLang config: %w", err)
	}
	defer f.Close()
	decoder := yaml.NewDecoder(f)
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return NativeConfig{}, fmt.Errorf("decode SGLang config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return NativeConfig{}, errors.New("decode SGLang config: multiple YAML documents")
		}
		return NativeConfig{}, fmt.Errorf("decode SGLang config: %w", err)
	}
	var options map[string]any
	if err := document.Decode(&options); err != nil {
		return NativeConfig{}, fmt.Errorf("decode SGLang options: %w", err)
	}
	if err := validateNativeOptions(options); err != nil {
		return NativeConfig{}, err
	}
	var cfg NativeConfig
	if err := document.Decode(&cfg); err != nil {
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

// SGLang turns YAML keys, scalar values, and list items into CLI arguments.
// Reject embedded flags and require full names for fields ARIES consumes so
// native execution cannot override the endpoint or GPU topology we validate.
func validateNativeOptions(options map[string]any) error {
	for option, value := range options {
		if strings.Contains(option, "=") {
			return errors.New("SGLang option keys must not contain '='")
		}
		values, ok := value.([]any)
		if !ok {
			values = []any{value}
		}
		for _, value := range values {
			if strings.HasPrefix(fmt.Sprint(value), "-") {
				return fmt.Errorf("SGLang option %q values must not start with '-'", option)
			}
		}
		if option == "" {
			continue
		}
		for _, names := range [][]string{
			{"served-model-name"}, {"host"}, {"port"}, {"device"},
			{"tensor-parallel-size", "tp-size"},
			{"pipeline-parallel-size", "pp-size"},
			{"data-parallel-size", "dp-size"},
			{"enable-dp-attention"},
			{"expert-parallel-size", "ep-size"},
			{"attention-context-parallel-size", "attn-cp-size"},
			{"moe-data-parallel-size", "moe-dp-size"},
			{"nnodes"}, {"node-rank"},
		} {
			if option == names[0] {
				continue
			}
			for _, name := range names {
				if strings.HasPrefix(name, option) {
					return fmt.Errorf("SGLang option %q must use the full name %q for ARIES endpoint and GPU validation", option, names[0])
				}
			}
		}
	}
	return nil
}

func (c NativeConfig) validate() error {
	required := []struct{ name, value string }{
		{"served-model-name", c.ServedModelName},
		{"host", c.Host}, {"device", c.Device},
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
