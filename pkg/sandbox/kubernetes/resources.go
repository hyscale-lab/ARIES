package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Pod-level telemetry is read from the kubelet Summary API
// (/api/v1/nodes/<node>/proxy/stats/summary) rather than from metrics-server.
//
// Two reasons, both load-bearing:
//
//   - Semantics. The monitor contract wants a monotonically increasing
//     CPUUsageNanoseconds and does the rate arithmetic itself, deployment-neutral
//     (see pkg/monitor). The Summary API reports exactly that as
//     usageCoreNanoSeconds, matching Docker's CPUStats.CPUUsage.TotalUsage.
//     metrics-server reports a pre-averaged instantaneous rate in millicores,
//     which cannot be converted back into a cumulative counter without losing
//     the very detail the monitor exists to capture.
//
//   - Cost. One request per *node* returns every pod on that node, so sampling
//     cost scales with the node count, not the task count. The alternative —
//     reading cgroup files through `kubectl exec` — would put a process spawn,
//     a TLS handshake and a SPDY upgrade on every pod on every tick.
//
// The tradeoff is RBAC: reading the Summary API needs `get` on the
// cluster-scoped `nodes/proxy` subresource, which a namespaced Role cannot
// grant. See k8s/base/rbac.yaml.

const summaryTimeout = 20 * time.Second

// ResourceOptions scope one Kubernetes resource source to a single ARIES run.
type ResourceOptions struct {
	RunID       string
	TaskIDs     []string
	Namespace   string
	KubectlPath string
}

// KubeResourceSource collects raw cumulative counters for the pods belonging to
// one run. Rate calculation and artifact writing stay in pkg/monitor.
type KubeResourceSource struct {
	kubectl   string
	namespace string
	runID     string
	tasks     map[string]struct{}

	// lastObserved is the newest kubelet observation time already emitted per
	// runtime, used to suppress repeats. See Sample.
	mu           sync.Mutex
	lastObserved map[string]time.Time
}

// NewResourceSource constructs a Kubernetes resource source without contacting
// the cluster.
func NewResourceSource(options ResourceOptions) (*KubeResourceSource, error) {
	if err := validateIdentity("run", options.RunID); err != nil {
		return nil, err
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("kubernetes resource source requires at least one task ID")
	}
	tasks := make(map[string]struct{}, len(options.TaskIDs))
	for _, taskID := range options.TaskIDs {
		if err := validateIdentity("task", taskID); err != nil {
			return nil, err
		}
		if _, exists := tasks[taskID]; exists {
			return nil, fmt.Errorf("kubernetes resource task ID %q is repeated", taskID)
		}
		tasks[taskID] = struct{}{}
	}
	if options.Namespace == "" {
		options.Namespace = defaultNamespace
	}
	if options.KubectlPath == "" {
		options.KubectlPath = defaultKubectl
	}
	return &KubeResourceSource{
		kubectl: options.KubectlPath, namespace: options.Namespace,
		runID: options.RunID, tasks: tasks,
		lastObserved: map[string]time.Time{},
	}, nil
}

// Close releases source-level resources. The kubectl backend holds none.
func (source *KubeResourceSource) Close() error { return nil }

// BaselineGracePeriod implements monitor.BaselineTolerantSource.
//
// This source emits a reading only when the kubelet's observation time advances
// (see observationIsNew), so at the monitor's 1s interval a live pod is absent
// from most samples. The recorder's default is to discard a runtime's CPU
// baseline the moment it is absent, which would make every emitted sample look
// like that runtime's first and report cpu_percent 0 forever while the raw
// counter climbed.
//
// 30 samples covers the observed 10-20s kubelet housekeeping cadence with room
// for jitter, while still discarding the baseline for a pod that has genuinely
// gone within about half a minute.
func (source *KubeResourceSource) BaselineGracePeriod() int { return 30 }

// resourceTarget is one ARIES-owned pod resolved from the API server, carrying
// the identity the Summary API does not report.
type resourceTarget struct {
	podName   string
	namespace string
	node      string
	taskID    string
	component string
	limit     uint64
}

// Sample discovers this run's pods, then reads each involved node's kubelet
// summary once and joins the two.
func (source *KubeResourceSource) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	targets, err := source.discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}
	// Group by node so each kubelet is queried once per tick regardless of how
	// many of this run's pods happen to be on it.
	byNode := map[string][]resourceTarget{}
	nodes := make([]string, 0, 4)
	for _, target := range targets {
		if _, seen := byNode[target.node]; !seen {
			nodes = append(nodes, target.node)
		}
		byNode[target.node] = append(byNode[target.node], target)
	}
	sort.Strings(nodes)

	readings := make([]core.ResourceReading, 0, len(targets))
	for _, node := range nodes {
		stats, err := source.nodeSummary(ctx, node)
		if err != nil {
			// A node that stops answering mid-run must not fail the run. The
			// gap shows up as missing samples, which is honest; aborting the
			// experiment over a telemetry hiccup would not be.
			continue
		}
		for _, target := range byNode[node] {
			reading, ok := stats.reading(target)
			if !ok {
				// Pod finished between discovery and the summary read, or the
				// kubelet has not yet produced a cAdvisor sample for it.
				continue
			}
			if !source.observationIsNew(reading.RuntimeID, reading.ObservedAt) {
				continue
			}
			readings = append(readings, reading)
		}
	}
	sort.Slice(readings, func(i, j int) bool {
		if readings[i].TaskID != readings[j].TaskID {
			return readings[i].TaskID < readings[j].TaskID
		}
		if readings[i].Component != readings[j].Component {
			return readings[i].Component < readings[j].Component
		}
		return readings[i].RuntimeID < readings[j].RuntimeID
	})
	return readings, nil
}

// observationIsNew reports whether this runtime's kubelet observation time has
// advanced since the last reading emitted for it, recording it when it has.
//
// This exists because the kubelet does not compute stats on demand. The Summary
// API serves whatever its cAdvisor housekeeping loop last cached, which by
// default refreshes about every 10s. Polling it at the monitor's 1s interval
// therefore returns the *same* observation, timestamp included, ten times over.
//
// pkg/monitor divides the CPU counter delta by the wall-clock delta between
// consecutive observations, so a repeated timestamp is a division by zero. It
// rejects the reading and fails the whole monitor for that task — which is
// exactly what happened before this filter existed: `sample_count: 1` followed
// by "observation time did not advance".
//
// Suppressing repeats here rather than raising the monitor's interval keeps the
// contract deployment-neutral: the Docker backend genuinely can sample every
// second, because ContainerStats computes fresh values per call. The practical
// consequence is that Kubernetes runs resolve to the kubelet's housekeeping
// cadence no matter what interval is configured.
func (source *KubeResourceSource) observationIsNew(runtimeID string, observed time.Time) bool {
	source.mu.Lock()
	defer source.mu.Unlock()
	if previous, seen := source.lastObserved[runtimeID]; seen && !observed.After(previous) {
		return false
	}
	source.lastObserved[runtimeID] = observed
	return true
}

// discover lists the ARIES-owned pods for this run. The run and task IDs live
// in annotations rather than labels (they may exceed the 63-byte label-value
// limit), so the label selector narrows to ARIES-managed pods and the
// annotations are matched client-side.
func (source *KubeResourceSource) discover(ctx context.Context) ([]resourceTarget, error) {
	out, err := source.run(ctx, "get", "pods", "-n", source.namespace,
		"-l", "app.kubernetes.io/managed-by=aries", "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("list kubernetes resource pods: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				NodeName   string `json:"nodeName"`
				Containers []struct {
					Resources struct {
						Limits map[string]string `json:"limits"`
					} `json:"resources"`
				} `json:"containers"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode kubernetes resource pod list: %w", err)
	}
	targets := make([]resourceTarget, 0, len(list.Items))
	for _, item := range list.Items {
		if item.Metadata.Annotations["aries.dev/run-id"] != source.runID {
			continue
		}
		taskID := item.Metadata.Annotations["aries.dev/task-id"]
		if _, selected := source.tasks[taskID]; !selected {
			continue
		}
		component, supported := resourceComponent(item.Metadata.Labels["app.kubernetes.io/component"])
		// Only Running pods report counters; Pending and Succeeded ones do not.
		if !supported || item.Status.Phase != "Running" || item.Spec.NodeName == "" {
			continue
		}
		var limit uint64
		if len(item.Spec.Containers) > 0 {
			limit = parseMemoryQuantity(item.Spec.Containers[0].Resources.Limits["memory"])
		}
		targets = append(targets, resourceTarget{
			podName: item.Metadata.Name, namespace: item.Metadata.Namespace,
			node: item.Spec.NodeName, taskID: taskID, component: component, limit: limit,
		})
	}
	return targets, nil
}

// nodeSummary reads one kubelet's stats summary through the API server proxy.
func (source *KubeResourceSource) nodeSummary(ctx context.Context, node string) (*nodeStats, error) {
	out, err := source.run(ctx, "get", "--raw",
		"/api/v1/nodes/"+node+"/proxy/stats/summary")
	if err != nil {
		return nil, fmt.Errorf("read kubelet summary for node %s: %w", node, err)
	}
	var stats nodeStats
	if err := json.Unmarshal(out, &stats); err != nil {
		return nil, fmt.Errorf("decode kubelet summary for node %s: %w", node, err)
	}
	return &stats, nil
}

// nodeStats is the subset of the kubelet Summary API this source consumes.
type nodeStats struct {
	Pods []struct {
		PodRef struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"podRef"`
		CPU struct {
			Time                 time.Time `json:"time"`
			UsageCoreNanoSeconds *uint64   `json:"usageCoreNanoSeconds"`
		} `json:"cpu"`
		Memory struct {
			Time            time.Time `json:"time"`
			WorkingSetBytes *uint64   `json:"workingSetBytes"`
			AvailableBytes  *uint64   `json:"availableBytes"`
		} `json:"memory"`
	} `json:"pods"`
}

// reading joins one discovered pod against this node's summary.
func (stats *nodeStats) reading(target resourceTarget) (core.ResourceReading, bool) {
	for _, pod := range stats.Pods {
		if pod.PodRef.Name != target.podName || pod.PodRef.Namespace != target.namespace {
			continue
		}
		if pod.CPU.UsageCoreNanoSeconds == nil || pod.Memory.WorkingSetBytes == nil {
			return core.ResourceReading{}, false
		}
		observed := pod.CPU.Time
		if observed.IsZero() {
			observed = pod.Memory.Time
		}
		if observed.IsZero() {
			return core.ResourceReading{}, false
		}
		// workingSetBytes is the field the kubelet's own eviction logic uses,
		// so it is the number that decides whether this pod gets OOM-killed —
		// the closest analogue to Docker's MemoryStats.Usage.
		usage := *pod.Memory.WorkingSetBytes
		limit := target.limit
		if limit == 0 && pod.Memory.AvailableBytes != nil {
			// No explicit limit in the spec: the kubelet still reports headroom
			// against the effective ceiling, so recover it as usage+available.
			limit = usage + *pod.Memory.AvailableBytes
		}
		return core.ResourceReading{
			TaskID:    target.taskID,
			Component: target.component,
			// The pod name alone, not "<namespace>/<pod>". pkg/monitor uses the
			// runtime ID as a path component and rejects "/" as a traversal
			// guard, so a namespaced ID fails validation and the whole monitor
			// reports status "failed" with zero samples. The Docker backend
			// never trips this because container IDs are hex. Pod names are
			// unique within a namespace, and the monitor is already scoped to
			// one run in one namespace, so nothing is lost.
			RuntimeID:           target.podName,
			RuntimeName:         target.podName,
			ObservedAt:          observed,
			CPUUsageNanoseconds: *pod.CPU.UsageCoreNanoSeconds,
			MemoryUsageBytes:    usage,
			MemoryLimitBytes:    limit,
		}, true
	}
	return core.ResourceReading{}, false
}

func (source *KubeResourceSource) run(ctx context.Context, args ...string) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()
	cmd := exec.CommandContext(callCtx, source.kubectl, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return []byte(stdout.String()), nil
}

// resourceComponent mirrors the Docker source's component filter, so the two
// backends label readings identically in resources.jsonl.
func resourceComponent(component string) (string, bool) {
	switch component {
	case "sandbox", "harness":
		return component, true
	default:
		return "", false
	}
}

// parseMemoryQuantity reads the binary-suffixed memory quantities this backend
// writes (containerResources emits "<n>Mi"). It is deliberately narrow: an
// unrecognised form yields 0, which makes the caller fall back to the kubelet's
// reported headroom rather than record a wrong limit.
func parseMemoryQuantity(value string) uint64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	multipliers := []struct {
		suffix string
		scale  uint64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000},
	}
	for _, m := range multipliers {
		if !strings.HasSuffix(value, m.suffix) {
			continue
		}
		digits := strings.TrimSuffix(value, m.suffix)
		parsed, err := parseUint(digits)
		if err != nil {
			return 0
		}
		return parsed * m.scale
	}
	parsed, err := parseUint(value)
	if err != nil {
		return 0
	}
	return parsed
}

func parseUint(value string) (uint64, error) {
	if value == "" {
		return 0, errors.New("empty quantity")
	}
	var out uint64
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric quantity %q", value)
		}
		out = out*10 + uint64(r-'0')
	}
	return out, nil
}
