package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	containertypes "github.com/moby/moby/api/types/container"
)

// fakeCAdvisorPrometheus serves the three instant queries from per-container
// values, keyed by the cgroup path cAdvisor reports for a Docker container.
type fakeCAdvisorPrometheus struct {
	mu      sync.Mutex
	cpu     map[string]float64
	memory  map[string]float64
	sampled map[string]float64
	queries []string
}

func (f *fakeCAdvisorPrometheus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	expression := r.URL.Query().Get("query")
	f.queries = append(f.queries, expression)
	values := f.cpu
	switch {
	case strings.Contains(expression, "timestamp("):
		values = f.sampled
	case strings.Contains(expression, "container_memory_working_set_bytes"):
		values = f.memory
	}
	var result []map[string]any
	for id, value := range values {
		result = append(result, map[string]any{"metric": map[string]string{"id": "/system.slice/docker-" + id + ".scope"}, "value": []any{1.0, strconv.FormatFloat(value, 'f', -1, 64)}})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
}

func newFakePrometheusSource(t *testing.T, prometheus *fakeCAdvisorPrometheus) (*PrometheusResourceSource, *fakeResourceAPI) {
	t.Helper()
	server := httptest.NewServer(prometheus)
	t.Cleanup(server.Close)
	source, err := NewPrometheusResourceSource(ResourceOptions{RunID: "run-1", TaskIDs: []string{"fix-git"}}, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	docker, fake := newFakeResourceSource()
	source.DockerResourceSource = docker
	return source, fake
}

func TestPrometheusResourceSourceReadsCAdvisorSeriesForTheRunsContainers(t *testing.T) {
	prometheus := &fakeCAdvisorPrometheus{
		cpu:     map[string]float64{resourceSandboxID: 1.5, resourceHarnessID: 0.25},
		memory:  map[string]float64{resourceSandboxID: 4096, resourceHarnessID: 2048},
		sampled: map[string]float64{resourceSandboxID: 1000.5, resourceHarnessID: 1000.5},
	}
	source, fake := newFakePrometheusSource(t, prometheus)
	sandbox := fake.inspections[resourceSandboxID]
	sandbox.HostConfig = &containertypes.HostConfig{Resources: containertypes.Resources{Memory: 4 << 30}}
	fake.inspections[resourceSandboxID] = sandbox

	readings, err := source.Sample(context.Background())
	if err != nil || len(readings) != 2 {
		t.Fatalf("readings = %+v, %v", readings, err)
	}
	harness, task := readings[0], readings[1]
	if task.Component != "sandbox" || task.RuntimeID != resourceSandboxID || task.RuntimeName != "aries-task" || task.TaskID != "fix-git" ||
		task.CPUUsageNanoseconds != 1_500_000_000 || task.MemoryUsageBytes != 4096 || task.MemoryLimitBytes != 4<<30 ||
		!task.ObservedAt.Equal(time.Unix(1000, 500_000_000)) {
		t.Fatalf("sandbox reading = %+v", task)
	}
	// No configured limit is reported as none, not as the host's memory.
	if harness.Component != "harness" || harness.MemoryLimitBytes != 0 {
		t.Fatalf("harness reading = %+v", harness)
	}
	if len(fake.statsOptions) != 0 {
		t.Fatal("the Docker stats API was read")
	}
	for _, query := range prometheus.queries {
		if !strings.Contains(query, `{container_label_aries_managed="true",container_label_aries_run="run-1"}`) {
			t.Fatalf("query %q is not limited to the run", query)
		}
	}

	// The same cAdvisor sample served again is not a new reading.
	if repeated, err := source.Sample(context.Background()); err != nil || len(repeated) != 0 {
		t.Fatalf("repeated = %+v, %v", repeated, err)
	}
	prometheus.mu.Lock()
	prometheus.cpu[resourceSandboxID], prometheus.sampled[resourceSandboxID] = 2.5, 1001.5
	prometheus.mu.Unlock()
	advanced, err := source.Sample(context.Background())
	if err != nil || len(advanced) != 1 || advanced[0].CPUUsageNanoseconds != 2_500_000_000 {
		t.Fatalf("advanced = %+v, %v", advanced, err)
	}
}

func TestPrometheusResourceSourceReportsAMissingCollector(t *testing.T) {
	prometheus := &fakeCAdvisorPrometheus{cpu: map[string]float64{}, memory: map[string]float64{}, sampled: map[string]float64{}}
	source, _ := newFakePrometheusSource(t, prometheus)
	// A container cAdvisor has not sampled yet is only a missing sample...
	for attempt := 1; attempt <= collectorGrace; attempt++ {
		if readings, err := source.Sample(context.Background()); err != nil || len(readings) != 0 {
			t.Fatalf("attempt %d = %+v, %v", attempt, readings, err)
		}
	}
	// ...but running containers never seen at all mean nothing is collecting.
	if _, err := source.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "no cAdvisor series") {
		t.Fatalf("missing collector = %v", err)
	}
	if _, err := NewPrometheusResourceSource(ResourceOptions{RunID: "run-1", TaskIDs: []string{"fix-git"}}, "prometheus:9090"); err == nil {
		t.Fatal("a malformed Prometheus address was accepted")
	}
}
