package nvidia

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const gpuQuery = "index,uuid,name,utilization.gpu,utilization.memory,memory.used,memory.total,power.draw,temperature.gpu"

type Options struct {
	TaskID     string
	GPUIndices []int
	Executable string
}

type commandRunner func(context.Context, string, []string) ([]byte, error)

// Source reads selected host GPU counters without controlling GPU allocation.
type Source struct {
	taskID     string
	executable string
	indices    []int
	run        commandRunner
	now        func() time.Time
}

func NewSource(options Options) (*Source, error) {
	if strings.TrimSpace(options.TaskID) == "" {
		return nil, errors.New("NVIDIA resource source task ID is required")
	}
	if len(options.GPUIndices) == 0 {
		return nil, errors.New("NVIDIA resource source requires at least one GPU index")
	}
	indices := append([]int(nil), options.GPUIndices...)
	sort.Ints(indices)
	for index, value := range indices {
		if value < 0 {
			return nil, errors.New("NVIDIA GPU indices must be non-negative")
		}
		if index > 0 && value == indices[index-1] {
			return nil, errors.New("NVIDIA GPU indices must not contain duplicates")
		}
	}
	executable := options.Executable
	if executable == "" {
		executable = "nvidia-smi"
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("locate NVIDIA monitor executable: %w", err)
	}
	return &Source{
		taskID: options.TaskID, executable: resolved, indices: indices,
		run: runCommand, now: time.Now,
	}, nil
}

func runCommand(ctx context.Context, executable string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, executable, args...).Output()
}

func (source *Source) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	selected := make([]string, len(source.indices))
	for index, gpu := range source.indices {
		selected[index] = strconv.Itoa(gpu)
	}
	args := []string{
		"--id=" + strings.Join(selected, ","),
		"--query-gpu=" + gpuQuery,
		"--format=csv,noheader,nounits",
	}
	output, err := source.run(ctx, source.executable, args)
	if err != nil {
		return nil, fmt.Errorf("query NVIDIA GPU resources: %w", err)
	}
	return source.parse(output)
}

func (source *Source) parse(output []byte) ([]core.ResourceReading, error) {
	reader := csv.NewReader(strings.NewReader(string(output)))
	reader.FieldsPerRecord = 9
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("decode NVIDIA GPU resources: %w", err)
	}
	if len(records) != len(source.indices) {
		return nil, errors.New("decode NVIDIA GPU resources: selected GPU count differs")
	}
	observedAt := source.now()
	expected := make(map[int]struct{}, len(source.indices))
	for _, index := range source.indices {
		expected[index] = struct{}{}
	}
	readings := make([]core.ResourceReading, 0, len(records))
	for recordIndex, record := range records {
		for index := range record {
			record[index] = strings.TrimSpace(record[index])
		}
		index, err := strconv.Atoi(record[0])
		if err != nil {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d index: %w", recordIndex, err)
		}
		if _, selected := expected[index]; !selected {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d: unselected index %d", recordIndex, index)
		}
		delete(expected, index)
		if record[1] == "" || record[2] == "" || strings.ContainsAny(record[1]+record[2], "\x00\r\n") {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d: invalid identity", recordIndex)
		}
		utilization, err := optionalFloat(record[3])
		if err != nil {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d utilization: %w", recordIndex, err)
		}
		memoryUtilization, err := optionalFloat(record[4])
		if err != nil {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d memory utilization: %w", recordIndex, err)
		}
		memoryUsage, err := mebibytes(record[5])
		if err != nil {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d memory usage: %w", recordIndex, err)
		}
		memoryTotal, err := mebibytes(record[6])
		if err != nil {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d total memory: %w", recordIndex, err)
		}
		power, err := optionalFloat(record[7])
		if err != nil {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d power: %w", recordIndex, err)
		}
		temperature, err := optionalFloat(record[8])
		if err != nil {
			return nil, fmt.Errorf("decode NVIDIA GPU record %d temperature: %w", recordIndex, err)
		}
		readings = append(readings, core.ResourceReading{
			TaskID: source.taskID, Component: "gpu", RuntimeID: record[1], RuntimeName: record[2], ObservedAt: observedAt,
			GPU: &core.GPUResourceReading{
				Index: index, UUID: record[1], UtilizationPercent: utilization,
				MemoryUtilizationPercent: memoryUtilization,
				MemoryUsageBytes:         memoryUsage, MemoryTotalBytes: memoryTotal,
				PowerWatts: power, TemperatureCelsius: temperature,
			},
		})
	}
	if len(expected) != 0 {
		return nil, errors.New("decode NVIDIA GPU resources: one or more selected GPUs are absent")
	}
	sort.Slice(readings, func(i, j int) bool { return readings[i].GPU.Index < readings[j].GPU.Index })
	return readings, nil
}

func optionalFloat(value string) (*float64, error) {
	if value == "N/A" || value == "[N/A]" {
		return nil, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return nil, errors.New("value is not finite")
	}
	return &parsed, nil
}

func mebibytes(value string) (uint64, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > float64(uint64(1<<60)/(1<<20)) {
		return 0, errors.New("value is outside the supported range")
	}
	return uint64(parsed * (1 << 20)), nil
}

func (*Source) Close() error { return nil }
