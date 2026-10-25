package docker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/internal/promquery"
	"github.com/moby/moby/client"
)

// PrometheusResourceSource reads a run's container counters from Prometheus,
// which scrapes the standalone cAdvisor that the ARIES chart runs on every
// node, rather than from the Docker stats API. The Kubernetes backend is
// measured from the same cAdvisor series, so the two backends' figures come
// from the same code at the same rate and can be compared.
//
// Which containers belong to the run is still decided through the Docker API,
// exactly as for DockerResourceSource; only the counters come from Prometheus.
type PrometheusResourceSource struct {
	*DockerResourceSource
	prometheus *promquery.Client
	selector   string

	mu sync.Mutex
	// lastObserved is the newest cAdvisor sample time emitted per container.
	lastObserved map[string]time.Time
	// limits caches each container's memory limit, which never changes.
	limits map[string]uint64
	// unmatched counts consecutive samples in which running containers had no
	// series at all, which means nothing is collecting them.
	unmatched int
}

// collectorGrace is how many consecutive samples may find running containers
// without any series before that is reported as a missing collector rather than
// a container cAdvisor has not sampled yet.
const collectorGrace = 30

// cgroupID finds the container ID in the cgroup path cAdvisor reports as the
// series' id label, such as /system.slice/docker-<id>.scope.
var cgroupID = regexp.MustCompile(`[0-9a-f]{64}`)

// NewPrometheusResourceSource constructs a source without contacting Docker or
// Prometheus.
func NewPrometheusResourceSource(options ResourceOptions, prometheusURL string) (*PrometheusResourceSource, error) {
	prometheus, err := promquery.New(prometheusURL)
	if err != nil {
		return nil, err
	}
	docker, err := NewResourceSource(options)
	if err != nil {
		prometheus.Close()
		return nil, err
	}
	return &PrometheusResourceSource{
		DockerResourceSource: docker, prometheus: prometheus,
		selector:     fmt.Sprintf(`{container_label_aries_managed="true",container_label_aries_run=%q}`, options.RunID),
		lastObserved: make(map[string]time.Time), limits: make(map[string]uint64),
	}, nil
}

func (source *PrometheusResourceSource) Close() error {
	source.prometheus.Close()
	return source.DockerResourceSource.Close()
}

// BaselineGracePeriod implements monitor.BaselineTolerantSource: a container
// is absent from a sample whenever cAdvisor and Prometheus have not both
// advanced since the last one.
func (source *PrometheusResourceSource) BaselineGracePeriod() int { return collectorGrace }

func (source *PrometheusResourceSource) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	runtimes, err := source.discover(ctx)
	if err != nil || len(runtimes) == 0 {
		return nil, err
	}
	cpu, err := source.prometheus.Vector(ctx, "sum by (id) (container_cpu_usage_seconds_total"+source.selector+")", "id")
	if err != nil {
		return nil, err
	}
	memory, err := source.prometheus.Vector(ctx, "sum by (id) (container_memory_working_set_bytes"+source.selector+")", "id")
	if err != nil {
		return nil, err
	}
	// An instant query is stamped with its evaluation time; the time cAdvisor
	// took the sample is what the rate arithmetic needs.
	sampled, err := source.prometheus.Vector(ctx, "max by (id) (timestamp(container_cpu_usage_seconds_total"+source.selector+"))", "id")
	if err != nil {
		return nil, err
	}
	byContainer := func(values map[string]float64) map[string]float64 {
		keyed := make(map[string]float64, len(values))
		for cgroup, value := range values {
			if id := cgroupID.FindString(cgroup); id != "" {
				keyed[id] = value
			}
		}
		return keyed
	}
	cpu, memory, sampled = byContainer(cpu), byContainer(memory), byContainer(sampled)

	source.mu.Lock()
	defer source.mu.Unlock()
	if len(cpu) == 0 {
		source.unmatched++
		if source.unmatched > collectorGrace {
			return nil, fmt.Errorf("Prometheus has no cAdvisor series for this run's %d running Docker containers; run the ARIES chart's cAdvisor on the Docker host", len(runtimes))
		}
		return nil, nil
	}
	source.unmatched = 0
	readings := make([]core.ResourceReading, 0, len(runtimes))
	for _, runtime := range runtimes {
		seconds, hasCPU := cpu[runtime.id]
		usage, hasMemory := memory[runtime.id]
		stamp, hasStamp := sampled[runtime.id]
		if !hasCPU || !hasMemory || !hasStamp || seconds < 0 || usage < 0 || stamp <= 0 {
			continue
		}
		observed := time.Unix(0, int64(stamp*float64(time.Second))).UTC()
		if previous, seen := source.lastObserved[runtime.id]; seen && !observed.After(previous) {
			continue
		}
		limit, err := source.memoryLimit(ctx, runtime.id)
		if errors.Is(err, errResourceRuntimeGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		source.lastObserved[runtime.id] = observed
		readings = append(readings, core.ResourceReading{
			Scope: source.scope, RunID: source.runID, TaskID: runtime.taskID, Component: runtime.component,
			RuntimeID: runtime.id, RuntimeName: runtime.name, ObservedAt: observed,
			CPUUsageNanoseconds: uint64(seconds*1e9 + 0.5), MemoryUsageBytes: uint64(usage), MemoryLimitBytes: limit,
		})
	}
	return readings, nil
}

// memoryLimit is the container's configured memory limit, or 0 when it has
// none, as the Kubernetes source reports a pod without one.
func (source *PrometheusResourceSource) memoryLimit(ctx context.Context, id string) (uint64, error) {
	if limit, cached := source.limits[id]; cached {
		return limit, nil
	}
	inspection, err := source.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return 0, errResourceRuntimeGone
	}
	if err != nil {
		return 0, fmt.Errorf("inspect Docker resource runtime %s: %w", id, err)
	}
	var limit uint64
	if inspection.Container.HostConfig != nil && inspection.Container.HostConfig.Memory > 0 {
		limit = uint64(inspection.Container.HostConfig.Memory)
	}
	source.limits[id] = limit
	return limit, nil
}
