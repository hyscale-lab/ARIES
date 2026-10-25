package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/internal/promquery"
)

// Pod telemetry is read from a Prometheus server that scrapes the standalone
// cAdvisor the ARIES chart runs on every node (templates/cadvisor.yaml), not
// the kubelet's built-in one: the kubelet's samples an idle container less and
// less often, so its readings arrive unevenly. The standalone one's series are
// told apart by the container labels it copies from containerd, which the
// kubelet's lack.
//
// Prometheus knows a pod by name only. Which run and task a pod belongs to is
// held in its annotations, so the source lists pods through kubectl, but only
// when Prometheus reports a pod name it has not classified. Annotations never
// change, so the steady state is HTTP alone.

const (
	// backstopTimeout bounds one kubectl call when the caller supplied no
	// deadline. The monitor wraps every Sample in a tighter request context.
	backstopTimeout = 20 * time.Second
	// discoveryFailureBudget is how many consecutive pod-listing failures are
	// absorbed before Sample reports one. A single Sample error ends the
	// monitor's loop for the rest of the run, so one slow call must not cost
	// all remaining telemetry. The budget stays small so revoked RBAC still
	// surfaces as an error.
	discoveryFailureBudget = 3
	// A pod can appear in Prometheus a moment before it reports Running. It is
	// looked up again after this long rather than ignored for good.
	unknownPodRetry = 15 * time.Second
	// podLabel and namespaceLabel are where cAdvisor puts containerd's pod
	// identity labels.
	podLabel       = "container_label_io_kubernetes_pod_name"
	namespaceLabel = "container_label_io_kubernetes_pod_namespace"
	containerLabel = "container_label_io_kubernetes_container_name"
)

// ResourceOptions scope one resource source to a single ARIES run.
type ResourceOptions struct {
	// Scope is "task" (default) for sandbox and harness pods of the selected
	// tasks, or "run" for the run's shared bridge pod.
	Scope   string
	RunID   string
	TaskIDs []string
	// PrometheusURL is the server's base address, reachable from the runner.
	PrometheusURL string
	Namespace     string
	KubectlPath   string
}

// ResourceSource collects raw cumulative counters for one run's pods. Rate
// calculation and artifact writing stay deployment-neutral in pkg/monitor.
type ResourceSource struct {
	kubectl    commandRunner
	prometheus *promquery.Client
	namespace  string
	scope      string
	runID      string
	tasks      map[string]struct{}

	mu sync.Mutex
	// lastObserved is the newest scrape time already emitted per pod.
	lastObserved      map[string]time.Time
	discoveryFailures int
	// collectorSeen is set once Prometheus has returned any series for the
	// namespace, which always holds the chart's own cAdvisor pods.
	collectorSeen bool
	// known maps a pod name to its identity. A pod that is not this source's
	// is remembered in unknownUntil so it is not listed again on every sample.
	known        map[string]resourceTarget
	unknownUntil map[string]time.Time
}

// NewResourceSource constructs a source without contacting the cluster or
// Prometheus.
func NewResourceSource(options ResourceOptions) (*ResourceSource, error) {
	if options.RunID == "" {
		return nil, errors.New("Kubernetes resource source requires a run ID")
	}
	if options.Scope == "" {
		options.Scope = "task"
	}
	if options.Scope != "task" && options.Scope != "run" {
		return nil, errors.New("invalid Kubernetes resource scope")
	}
	if options.Scope == "run" && len(options.TaskIDs) != 0 {
		return nil, errors.New("run resources cannot select task IDs")
	}
	if options.Scope == "task" && len(options.TaskIDs) == 0 {
		return nil, errors.New("Kubernetes resource source requires at least one task ID")
	}
	tasks := make(map[string]struct{}, len(options.TaskIDs))
	for _, taskID := range options.TaskIDs {
		if taskID == "" {
			return nil, errors.New("Kubernetes resource task ID is empty")
		}
		if _, exists := tasks[taskID]; exists {
			return nil, fmt.Errorf("Kubernetes resource task ID %q is repeated", taskID)
		}
		tasks[taskID] = struct{}{}
	}
	prometheus, err := promquery.New(options.PrometheusURL)
	if err != nil {
		return nil, err
	}
	if options.Namespace == "" {
		options.Namespace = defaultNamespace
	}
	if options.KubectlPath == "" {
		options.KubectlPath = defaultKubectl
	}
	return &ResourceSource{
		kubectl: execKubectl{path: options.KubectlPath}, prometheus: prometheus,
		namespace: options.Namespace, scope: options.Scope, runID: options.RunID, tasks: tasks,
		lastObserved: make(map[string]time.Time),
		known:        make(map[string]resourceTarget), unknownUntil: make(map[string]time.Time),
	}, nil
}

func (source *ResourceSource) Close() error {
	source.prometheus.Close()
	return nil
}

// BaselineGracePeriod implements monitor.BaselineTolerantSource. A reading is
// emitted only when the scraped sample is new, so a pod is absent from a
// sample whenever cAdvisor's housekeeping and Prometheus's scrape have not both
// advanced since the last one, and until its first scrape.
func (source *ResourceSource) BaselineGracePeriod() int { return 30 }

// resourceTarget is the identity of one ARIES-owned pod, which Prometheus does
// not carry.
type resourceTarget struct {
	podName   string
	taskID    string
	component string
	limit     uint64
}

// Sample reads every pod in the namespace from Prometheus and keeps the ones
// that belong to this source's run and tasks.
func (source *ResourceSource) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	// The container-name matchers drop the pod-level cgroup and the pause
	// container, leaving the containers that were started in the pod.
	selector := fmt.Sprintf(`{%s=%q,%s!="",%s!="POD"}`, namespaceLabel, source.namespace, containerLabel, containerLabel)
	byPod := "sum by (" + podLabel + ") "
	cpu, err := source.query(ctx, byPod+"(container_cpu_usage_seconds_total"+selector+")")
	if err != nil {
		return nil, err
	}
	if len(cpu) == 0 {
		// The chart's cAdvisor pods live in this namespace, so no series at
		// all means nothing is collecting, which must not read as idle pods.
		source.mu.Lock()
		seen := source.collectorSeen
		source.mu.Unlock()
		if !seen {
			return nil, fmt.Errorf("Prometheus has no standalone cAdvisor series for namespace %q; install the ARIES chart with cadvisor.enabled", source.namespace)
		}
		return nil, nil
	}
	source.mu.Lock()
	source.collectorSeen = true
	source.mu.Unlock()
	if err := source.identify(ctx, cpu); err != nil {
		return nil, err
	}
	memory, err := source.query(ctx, byPod+"(container_memory_working_set_bytes"+selector+")")
	if err != nil {
		return nil, err
	}
	// An instant query is stamped with its evaluation time. The time the
	// counter was scraped is what the rate arithmetic needs.
	scraped, err := source.query(ctx, "max by ("+podLabel+") (timestamp(container_cpu_usage_seconds_total"+selector+"))")
	if err != nil {
		return nil, err
	}
	readings := make([]core.ResourceReading, 0, len(cpu))
	for name, seconds := range cpu {
		source.mu.Lock()
		target, owned := source.known[name]
		source.mu.Unlock()
		usage, hasMemory := memory[name]
		stamp, hasStamp := scraped[name]
		if !owned || !hasMemory || !hasStamp || seconds < 0 || usage < 0 || stamp <= 0 {
			continue
		}
		observed := time.Unix(0, int64(stamp*float64(time.Second))).UTC()
		if !source.observationIsNew(name, observed) {
			continue
		}
		readings = append(readings, core.ResourceReading{
			Scope: source.scope, RunID: source.runID, TaskID: target.taskID, Component: target.component,
			// The pod name alone: the monitor uses the runtime ID as a path
			// component and rejects "/".
			RuntimeID: name, RuntimeName: name, ObservedAt: observed,
			CPUUsageNanoseconds: uint64(math.Round(seconds * 1e9)), MemoryUsageBytes: uint64(usage), MemoryLimitBytes: target.limit,
		})
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

// observationIsNew reports whether the scrape time advanced since the last
// reading emitted for this pod. Between kubelet refreshes Prometheus serves the
// same sample again; the monitor would divide a zero CPU delta by a zero
// interval and reject the reading.
func (source *ResourceSource) observationIsNew(runtimeID string, observed time.Time) bool {
	source.mu.Lock()
	defer source.mu.Unlock()
	if previous, seen := source.lastObserved[runtimeID]; seen && !observed.After(previous) {
		return false
	}
	source.lastObserved[runtimeID] = observed
	return true
}

// identify classifies pod names Prometheus reported that this source has not
// seen. It lists pods at most once per sample.
func (source *ResourceSource) identify(ctx context.Context, reported map[string]float64) error {
	now := time.Now()
	source.mu.Lock()
	pending := false
	for name := range reported {
		if _, owned := source.known[name]; !owned && now.After(source.unknownUntil[name]) {
			pending = true
			break
		}
	}
	source.mu.Unlock()
	if !pending {
		return nil
	}
	targets, err := source.discover(ctx)
	source.mu.Lock()
	defer source.mu.Unlock()
	if err != nil {
		source.discoveryFailures++
		if source.discoveryFailures <= discoveryFailureBudget {
			// Reported as missing readings so the monitor keeps sampling.
			return nil
		}
		return err
	}
	source.discoveryFailures = 0
	for _, target := range targets {
		source.known[target.podName] = target
		delete(source.unknownUntil, target.podName)
	}
	for name := range reported {
		if _, owned := source.known[name]; !owned {
			source.unknownUntil[name] = now.Add(unknownPodRetry)
		}
	}
	return nil
}

// discover lists this run's running pods. Run and task IDs are annotations, so
// the label selector narrows to ARIES pods and the rest is matched here.
func (source *ResourceSource) discover(ctx context.Context) ([]resourceTarget, error) {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, backstopTimeout)
		defer cancel()
	}
	var stdout, stderr bytes.Buffer
	if err := source.kubectl.Run(ctx, nil, &stdout, &stderr, "get", "pods", "-n", source.namespace, "-l", managedByLabel+"=aries", "-o", "json"); err != nil {
		return nil, fmt.Errorf("list Kubernetes resource pods: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var list struct {
		Items []podObject `json:"items"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		return nil, fmt.Errorf("decode Kubernetes resource pod list: %w", err)
	}
	targets := make([]resourceTarget, 0, len(list.Items))
	for _, pod := range list.Items {
		annotations := pod.Metadata.Annotations
		if annotations["aries.managed"] != "true" || annotations["aries.run"] != source.runID {
			continue
		}
		taskID := annotations["aries.task"]
		component := annotations["aries.component"]
		selected := false
		switch component {
		case "sandbox", "evaluation-sandbox", "harness":
			_, selected = source.tasks[taskID]
			selected = selected && source.scope == "task"
		case "bridge":
			selected = source.scope == "run" && taskID == "" && annotations["aries.kind"] == "tool-bridge"
		}
		// A pod that is not Running yet is classified on a later sample.
		if !selected || pod.Status.Phase != "Running" {
			continue
		}
		var limit uint64
		if len(pod.Spec.Containers) > 0 {
			limit = parseMemoryQuantity(pod.Spec.Containers[0].Resources.Limits["memory"])
		}
		targets = append(targets, resourceTarget{podName: pod.Metadata.Name, taskID: taskID, component: component, limit: limit})
	}
	return targets, nil
}

// query runs one instant query and returns its vector keyed by pod name.
func (source *ResourceSource) query(ctx context.Context, expression string) (map[string]float64, error) {
	return source.prometheus.Vector(ctx, expression, podLabel)
}

// parseMemoryQuantity reads the memory quantities this provider writes. An
// unrecognised form yields 0, which the artifact records as no known limit.
func parseMemoryQuantity(value string) uint64 {
	value = strings.TrimSpace(value)
	for _, unit := range []struct {
		suffix string
		scale  uint64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"k", 1000}, {"M", 1000 * 1000}, {"G", 1000 * 1000 * 1000}, {"", 1}} {
		if digits, found := strings.CutSuffix(value, unit.suffix); found {
			parsed, err := strconv.ParseUint(digits, 10, 64)
			if err != nil {
				return 0
			}
			return parsed * unit.scale
		}
	}
	return 0
}
