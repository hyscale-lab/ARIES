package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	managedLabelValue = "true"
	taskContainerKind = "task-container"
	harnessKind       = "openclaw-harness"
	maxMemoryBytes    = uint64(1 << 60)
	maxOnlineCPUs     = uint32(4096)
	maxCPUPercent     = float64(1_000_000)
	dockerUserAgent   = "aries-monitor/1"
)

var errContainerGone = errors.New("container disappeared during sampling")

type statsValidationError struct {
	err error
}

func (failure *statsValidationError) Error() string {
	return failure.err.Error()
}

func (failure *statsValidationError) Unwrap() error {
	return failure.err
}

// dockerAPI is deliberately read-only: monitoring must never control a
// container's lifecycle.
type dockerAPI interface {
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStats(context.Context, string, client.ContainerStatsOptions) (client.ContainerStatsResult, error)
	Close() error
}

type engineClient struct {
	api dockerAPI
}

type listedContainer struct {
	ID     string
	Name   string
	Labels map[string]string
}

type sampledContainer struct {
	taskID   string
	kind     string
	id       string
	name     string
	cpu      float64
	memory   uint64
	memLimit uint64
}

func newEngineClient(socket string) (*engineClient, error) {
	api, err := client.New(
		client.WithHost("unix://"+socket),
		client.WithUserAgent(dockerUserAgent),
	)
	if err != nil {
		return nil, err
	}
	return &engineClient{api: api}, nil
}

func (engine *engineClient) closeIdleConnections() {
	if engine.api != nil {
		_ = engine.api.Close()
	}
}

func (engine *engineClient) discover(ctx context.Context, runID string, tasks map[string]struct{}) ([]listedContainer, error) {
	filters := make(client.Filters).Add(
		"label",
		"aries.managed="+managedLabelValue,
		"aries.run="+runID,
	)
	result, err := engine.api.ContainerList(ctx, client.ContainerListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("list monitored Docker containers: %w", err)
	}

	allowed := make([]listedContainer, 0, len(result.Items))
	seen := make(map[string]struct{}, len(result.Items))
	for index, summary := range result.Items {
		if err := validateLabels(summary.Labels, runID); err != nil {
			return nil, fmt.Errorf("Docker container list record %d: %w", index, err)
		}
		kind := summary.Labels["aries.kind"]
		taskID := summary.Labels["aries.task"]
		_, taskAllowed := tasks[taskID]
		kindAllowed := kind == taskContainerKind || kind == harnessKind
		if !taskAllowed || !kindAllowed || summary.State != containertypes.StateRunning {
			continue
		}
		if summary.ID == "" || len(summary.Names) != 1 || !strings.HasPrefix(summary.Names[0], "/") {
			return nil, fmt.Errorf("Docker container list record %d has an invalid identity", index)
		}
		name := strings.TrimPrefix(summary.Names[0], "/")
		if name == "" {
			return nil, fmt.Errorf("Docker container list record %d has an invalid identity", index)
		}
		if _, duplicate := seen[summary.ID]; duplicate {
			return nil, fmt.Errorf("Docker container list repeats ID %s", summary.ID)
		}

		inspection, err := engine.api.ContainerInspect(ctx, summary.ID, client.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect monitored Docker container %s: %w", summary.ID, err)
		}
		if err := validateInspection(inspection.Container, summary.ID, name, summary.Labels, runID); err != nil {
			return nil, fmt.Errorf("validate monitored Docker container %s: %w", summary.ID, err)
		}

		seen[summary.ID] = struct{}{}
		allowed = append(allowed, listedContainer{ID: summary.ID, Name: name, Labels: summary.Labels})
	}
	sort.Slice(allowed, func(i, j int) bool {
		left, right := allowed[i].Labels, allowed[j].Labels
		if left["aries.task"] != right["aries.task"] {
			return left["aries.task"] < right["aries.task"]
		}
		if left["aries.kind"] != right["aries.kind"] {
			return left["aries.kind"] < right["aries.kind"]
		}
		return allowed[i].ID < allowed[j].ID
	})
	return allowed, nil
}

func validateLabels(labels map[string]string, runID string) error {
	if labels == nil || labels["aries.managed"] != managedLabelValue || labels["aries.run"] != runID {
		return errors.New("wrong ARIES ownership labels")
	}
	return nil
}

func validateInspection(
	inspection containertypes.InspectResponse,
	id string,
	name string,
	labels map[string]string,
	runID string,
) error {
	if inspection.ID != id || inspection.Name != "/"+name {
		return errors.New("identity differs from the container list record")
	}
	if inspection.State == nil || !inspection.State.Running || inspection.State.Status != containertypes.StateRunning {
		return errors.New("container is not running")
	}
	if inspection.Config == nil {
		return errors.New("container configuration is absent")
	}
	if err := validateLabels(inspection.Config.Labels, runID); err != nil {
		return err
	}
	for _, key := range []string{"aries.managed", "aries.run", "aries.task", "aries.kind"} {
		if inspection.Config.Labels[key] != labels[key] {
			return fmt.Errorf("label %q differs from the container list record", key)
		}
	}
	return nil
}

func (engine *engineClient) stats(ctx context.Context, container listedContainer) (sampledContainer, error) {
	result, err := engine.api.ContainerStats(ctx, container.ID, client.ContainerStatsOptions{
		Stream:                false,
		IncludePreviousSample: false,
	})
	if errdefs.IsNotFound(err) {
		return sampledContainer{}, errContainerGone
	}
	if err != nil {
		return sampledContainer{}, fmt.Errorf("read Docker stats for %s: %w", container.ID, err)
	}
	defer result.Body.Close()

	var document containertypes.StatsResponse
	decoder := json.NewDecoder(result.Body)
	if err := decoder.Decode(&document); err != nil {
		return sampledContainer{}, fmt.Errorf("decode Docker stats for %s: %w", container.ID, err)
	}

	cpuPercent, memory, limit, err := validateStats(document)
	if err != nil {
		return sampledContainer{}, fmt.Errorf("validate Docker stats for %s: %w", container.ID, &statsValidationError{err: err})
	}
	return sampledContainer{
		taskID:   container.Labels["aries.task"],
		kind:     container.Labels["aries.kind"],
		id:       container.ID,
		name:     container.Name,
		cpu:      cpuPercent,
		memory:   memory,
		memLimit: limit,
	}, nil
}

func validateStats(document containertypes.StatsResponse) (float64, uint64, uint64, error) {
	total := document.CPUStats.CPUUsage.TotalUsage
	system := document.CPUStats.SystemUsage
	online := document.CPUStats.OnlineCPUs
	if online == 0 {
		online = uint32(len(document.CPUStats.CPUUsage.PercpuUsage))
	}
	if online == 0 || online > maxOnlineCPUs {
		return 0, 0, 0, fmt.Errorf("online CPU count %d is outside the bound", online)
	}
	memory := document.MemoryStats.Usage
	limit := document.MemoryStats.Limit
	if memory > maxMemoryBytes || limit > maxMemoryBytes {
		return 0, 0, 0, errors.New("memory measurement exceeds the bound")
	}

	preTotal, preSystem, baselineAvailable := cpuBaseline(document.PreCPUStats)
	if !baselineAvailable {
		return 0, memory, limit, nil
	}
	if total < preTotal || system < preSystem {
		return 0, 0, 0, errors.New("CPU counters decreased")
	}
	cpuDelta := total - preTotal
	systemDelta := system - preSystem
	cpuPercent := float64(0)
	if cpuDelta != 0 {
		if systemDelta == 0 {
			return 0, 0, 0, errors.New("nonzero CPU delta has no system delta")
		}
		cpuPercent = float64(cpuDelta) / float64(systemDelta) * float64(online) * 100
	}
	if math.IsNaN(cpuPercent) || math.IsInf(cpuPercent, 0) || cpuPercent < 0 || cpuPercent > maxCPUPercent {
		return 0, 0, 0, fmt.Errorf("CPU percentage %v is invalid", cpuPercent)
	}
	return cpuPercent, memory, limit, nil
}

func cpuBaseline(previous containertypes.CPUStats) (uint64, uint64, bool) {
	if previous.CPUUsage.TotalUsage == 0 || previous.SystemUsage == 0 {
		return 0, 0, false
	}
	return previous.CPUUsage.TotalUsage, previous.SystemUsage, true
}

func requestContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= timeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}
