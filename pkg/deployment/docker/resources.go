package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const dockerResourceUserAgent = "aries-docker-resources/1"

var errResourceRuntimeGone = errors.New("resource runtime disappeared during sampling")

// ResourceOptions scope one Docker resource source to a single ARIES run.
type ResourceOptions struct {
	RunID        string
	TaskIDs      []string
	DockerSocket string
}

type resourceAPI interface {
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStats(context.Context, string, client.ContainerStatsOptions) (client.ContainerStatsResult, error)
	Close() error
}

// DockerResourceSource collects raw cumulative counters through the official
// Engine SDK. Rate calculation and artifact writing stay deployment-neutral in
// pkg/monitor.
type DockerResourceSource struct {
	api   resourceAPI
	runID string
	tasks map[string]struct{}
}

type resourceRuntime struct {
	id        string
	name      string
	taskID    string
	component string
}

func NewResourceSource(options ResourceOptions) (*DockerResourceSource, error) {
	if err := validateIdentity("run", options.RunID); err != nil {
		return nil, err
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("Docker resource source requires at least one task ID")
	}
	tasks := make(map[string]struct{}, len(options.TaskIDs))
	for _, taskID := range options.TaskIDs {
		if err := validateIdentity("task", taskID); err != nil {
			return nil, err
		}
		if _, exists := tasks[taskID]; exists {
			return nil, fmt.Errorf("Docker resource task ID %q is repeated", taskID)
		}
		tasks[taskID] = struct{}{}
	}
	socket := options.DockerSocket
	if socket == "" {
		socket = defaultDockerSocket
	}
	if !strings.Contains(socket, "://") {
		absolute, err := filepath.Abs(socket)
		if err != nil {
			return nil, fmt.Errorf("resolve Docker resource socket: %w", err)
		}
		socket = "unix://" + absolute
	}
	api, err := client.New(client.WithHost(socket), client.WithUserAgent(dockerResourceUserAgent))
	if err != nil {
		return nil, fmt.Errorf("create Docker resource client: %w", err)
	}
	return &DockerResourceSource{api: api, runID: options.RunID, tasks: tasks}, nil
}

func (source *DockerResourceSource) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	runtimes, err := source.discover(ctx)
	if err != nil {
		return nil, err
	}
	readings := make([]core.ResourceReading, 0, len(runtimes))
	for _, runtime := range runtimes {
		reading, err := source.read(ctx, runtime)
		if errors.Is(err, errResourceRuntimeGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		readings = append(readings, reading)
	}
	return readings, nil
}

func (source *DockerResourceSource) Close() error {
	if source == nil || source.api == nil {
		return nil
	}
	return source.api.Close()
}

func (source *DockerResourceSource) discover(ctx context.Context) ([]resourceRuntime, error) {
	filters := make(client.Filters).Add("label", "aries.managed=true", "aries.run="+source.runID)
	result, err := source.api.ContainerList(ctx, client.ContainerListOptions{Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("list Docker resource runtimes: %w", err)
	}
	seen := make(map[string]struct{}, len(result.Items))
	runtimes := make([]resourceRuntime, 0, len(result.Items))
	for index, summary := range result.Items {
		if err := validateResourceLabels(summary.Labels, source.runID); err != nil {
			return nil, fmt.Errorf("Docker resource list record %d: %w", index, err)
		}
		taskID := summary.Labels["aries.task"]
		component, supported := resourceComponent(summary.Labels["aries.component"])
		_, selected := source.tasks[taskID]
		if !selected || !supported || summary.State != containertypes.StateRunning {
			continue
		}
		if summary.ID == "" || len(summary.Names) != 1 || !strings.HasPrefix(summary.Names[0], "/") {
			return nil, fmt.Errorf("Docker resource list record %d has an invalid identity", index)
		}
		name := strings.TrimPrefix(summary.Names[0], "/")
		if name == "" {
			return nil, fmt.Errorf("Docker resource list record %d has an invalid identity", index)
		}
		if _, duplicate := seen[summary.ID]; duplicate {
			return nil, fmt.Errorf("Docker resource list repeats ID %s", summary.ID)
		}
		inspection, err := source.api.ContainerInspect(ctx, summary.ID, client.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect Docker resource runtime %s: %w", summary.ID, err)
		}
		if err := validateResourceInspection(inspection.Container, summary.ID, name, summary.Labels, source.runID); errors.Is(err, errResourceRuntimeGone) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("validate Docker resource runtime %s: %w", summary.ID, err)
		}
		seen[summary.ID] = struct{}{}
		runtimes = append(runtimes, resourceRuntime{id: summary.ID, name: name, taskID: taskID, component: component})
	}
	sort.Slice(runtimes, func(i, j int) bool {
		if runtimes[i].taskID != runtimes[j].taskID {
			return runtimes[i].taskID < runtimes[j].taskID
		}
		if runtimes[i].component != runtimes[j].component {
			return runtimes[i].component < runtimes[j].component
		}
		return runtimes[i].id < runtimes[j].id
	})
	return runtimes, nil
}

func (source *DockerResourceSource) read(ctx context.Context, runtime resourceRuntime) (core.ResourceReading, error) {
	result, err := source.api.ContainerStats(ctx, runtime.id, client.ContainerStatsOptions{Stream: false, IncludePreviousSample: false})
	if errdefs.IsNotFound(err) {
		return core.ResourceReading{}, errResourceRuntimeGone
	}
	if err != nil {
		return core.ResourceReading{}, fmt.Errorf("read Docker resource stats for %s: %w", runtime.id, err)
	}
	defer result.Body.Close()
	var document containertypes.StatsResponse
	if err := json.NewDecoder(result.Body).Decode(&document); err != nil {
		return core.ResourceReading{}, fmt.Errorf("decode Docker resource stats for %s: %w", runtime.id, err)
	}
	if document.Read.IsZero() {
		return core.ResourceReading{}, fmt.Errorf("Docker resource stats for %s omit observation time", runtime.id)
	}
	if document.MemoryStats.Usage == 0 && document.MemoryStats.Limit == 0 {
		return core.ResourceReading{}, errResourceRuntimeGone
	}
	return core.ResourceReading{
		TaskID: runtime.taskID, Component: runtime.component, RuntimeID: runtime.id, RuntimeName: runtime.name,
		ObservedAt: document.Read, CPUUsageNanoseconds: document.CPUStats.CPUUsage.TotalUsage,
		MemoryUsageBytes: document.MemoryStats.Usage, MemoryLimitBytes: document.MemoryStats.Limit,
	}, nil
}

func resourceComponent(component string) (string, bool) {
	switch component {
	case "sandbox", "harness":
		return component, true
	default:
		return "", false
	}
}

func validateResourceLabels(labels map[string]string, runID string) error {
	if labels == nil || labels["aries.managed"] != "true" || labels["aries.run"] != runID {
		return errors.New("wrong ARIES ownership labels")
	}
	return nil
}

func validateResourceInspection(inspection containertypes.InspectResponse, id, name string, labels map[string]string, runID string) error {
	if inspection.ID != id || inspection.Name != "/"+name {
		return errors.New("identity differs from the container list record")
	}
	if inspection.Config == nil {
		return errors.New("container configuration is absent")
	}
	if err := validateResourceLabels(inspection.Config.Labels, runID); err != nil {
		return err
	}
	for _, key := range []string{"aries.managed", "aries.run", "aries.task", "aries.kind", "aries.component"} {
		if inspection.Config.Labels[key] != labels[key] {
			return fmt.Errorf("label %q differs from the container list record", key)
		}
	}
	if inspection.State == nil {
		return errors.New("container state is absent")
	}
	if !inspection.State.Running || inspection.State.Status != containertypes.StateRunning {
		return errResourceRuntimeGone
	}
	return nil
}
