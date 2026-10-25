package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

func taskRequest(allowNetwork bool) deployment.TaskEnvironmentRequest {
	return deployment.TaskEnvironmentRequest{
		SandboxRequest: core.SandboxRequest{RunID: "run-1", TaskID: "task-1", Environment: core.Environment{AllowNetwork: allowNetwork}},
		RuntimeName:    "aries-sandbox-abc123",
	}
}

// Naming a direction in policyTypes with no rule block denies it, so the
// air-gapped policy must omit the egress key entirely.
func TestTaskPolicyDeniesIngressAndScopesEgress(t *testing.T) {
	manager, _ := newTestManager(t, Options{PodCIDR: "192.168.0.0/16", ServiceCIDR: "10.96.0.0/12"})
	for _, allow := range []bool{false, true} {
		raw, err := manager.taskPolicyManifest(taskRequest(allow), nil)
		if err != nil {
			t.Fatal(err)
		}
		spec := decodeManifest(t, raw)["spec"].(map[string]any)
		if selector := spec["podSelector"].(map[string]any)["matchLabels"].(map[string]any); len(selector) != 1 || selector[runtimeLabel] != "aries-sandbox-abc123" {
			t.Fatalf("selector = %v", selector)
		}
		if types := spec["policyTypes"].([]any); !slices.Equal(types, []any{"Ingress", "Egress"}) {
			t.Fatalf("policyTypes = %v", types)
		}
		if _, present := spec["ingress"]; present {
			t.Fatal("ingress rule would admit traffic")
		}
		egress, present := spec["egress"].([]any)
		if present != allow {
			t.Fatalf("allow=%v egress present=%v", allow, present)
		}
		if allow {
			block := egress[1].(map[string]any)["to"].([]any)[0].(map[string]any)["ipBlock"].(map[string]any)
			if block["cidr"] != "0.0.0.0/0" || !slices.Equal(block["except"].([]any), []any{"192.168.0.0/16", "10.96.0.0/12"}) {
				t.Fatalf("ipBlock = %v", block)
			}
		}
	}
}

func TestEnvironmentsOwnTheirPolicies(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	policies := map[string]bool{}
	fake.respond = func(_ context.Context, args []string, stdin []byte, stdout, _ io.Writer) error {
		switch {
		case args[0] == "create":
			name := decodeManifest(t, stdin)["metadata"].(map[string]any)["name"].(string)
			policies[name] = true
		case args[0] == "delete" && args[1] == "networkpolicy":
			delete(policies, args[4])
		case args[0] == "get" && args[1] == "networkpolicy" && policies[args[4]]:
			_, _ = io.WriteString(stdout, "networkpolicy.networking.k8s.io/"+args[4])
		}
		return nil
	}
	run := manager.NewRunEnvironment("run-1")
	placement, err := run.Start(context.Background())
	if err != nil || placement.AttachmentID != "aries" || run.NetworkName() != "aries" {
		t.Fatalf("Start = %+v, %v", placement, err)
	}
	if _, err := run.Start(context.Background()); err == nil {
		t.Fatal("run environment reused")
	}
	// The bridge accepts unauthenticated clients, so its policy keeps sandboxes out.
	bridge := decodeManifest(t, fake.inputs[0])["spec"].(map[string]any)
	if string(fake.inputs[0]) == "" || bridge["podSelector"].(map[string]any)["matchLabels"].(map[string]any)[componentLabel] != "bridge" || !strings.Contains(string(fake.inputs[0]), `"NotIn","values":["sandbox","evaluation-sandbox"]`) {
		t.Fatalf("bridge policy = %s", fake.inputs[0])
	}

	task := run.NewTaskEnvironment()
	connectivity, err := task.Start(context.Background(), taskRequest(false))
	if err != nil || connectivity.Placement.AttachmentID != "aries" || connectivity.SearchURL != "" {
		t.Fatalf("task Start = %+v, %v", connectivity, err)
	}
	if !policies["aries-sandbox-abc123"] || task.Validate(context.Background()) != nil {
		t.Fatalf("task policy missing: %v", policies)
	}
	if err := task.Stop(context.Background()); err != nil || policies["aries-sandbox-abc123"] {
		t.Fatalf("task Stop = %v, policies = %v", err, policies)
	}
	if err := task.Validate(context.Background()); err == nil {
		t.Fatal("stopped task environment validated")
	}
	if len(policies) != 1 {
		t.Fatalf("task Stop removed run connectivity: %v", policies)
	}
	if err := run.Stop(context.Background()); err != nil || len(policies) != 0 {
		t.Fatalf("run Stop = %v, policies = %v", err, policies)
	}
	if err := run.Stop(context.Background()); err != nil {
		t.Fatalf("repeated Stop = %v", err)
	}
}

func TestTaskEnvironmentRejectsForeignRuns(t *testing.T) {
	manager, _ := newTestManager(t, Options{})
	run := manager.NewRunEnvironment("run-1")
	if _, err := run.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	foreign := taskRequest(false)
	foreign.RunID = "run-2"
	if _, err := run.NewTaskEnvironment().Start(context.Background(), foreign); err == nil {
		t.Fatal("foreign run accepted")
	}
}

// A declared task service is the only way into a sandbox: harness pods may
// reach that one port through a Service named after the sandbox.
func TestTaskEnvironmentExposesADeclaredTaskService(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	objects := map[string]bool{}
	var manifests []map[string]any
	fake.respond = func(_ context.Context, args []string, stdin []byte, stdout, _ io.Writer) error {
		switch args[0] {
		case "create":
			manifest := decodeManifest(t, stdin)
			manifests = append(manifests, manifest)
			objects[strings.ToLower(manifest["kind"].(string))+"/"+manifest["metadata"].(map[string]any)["name"].(string)] = true
		case "delete":
			delete(objects, args[1]+"/"+args[4])
		case "get":
			if objects[args[1]+"/"+args[4]] {
				_, _ = io.WriteString(stdout, args[1]+"/"+args[4])
			}
		}
		return nil
	}
	run := manager.NewRunEnvironment("run-1")
	if _, err := run.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := taskRequest(true)
	request.Environment.Services.SearchPort = 8888
	request.Environment.Services.MCP = []core.TaskMCPService{{Name: "tools", Port: 9000, Path: "/mcp", Transport: "sse", TimeoutSeconds: 30}}
	task := run.NewTaskEnvironment()
	connectivity, err := task.Start(context.Background(), request)
	if err != nil || connectivity.SearchURL != "http://aries-sandbox-abc123:8888" {
		t.Fatalf("Start = %+v, %v", connectivity, err)
	}
	if len(connectivity.MCPServers) != 1 {
		t.Fatalf("MCP servers = %+v", connectivity.MCPServers)
	}
	if mcp := connectivity.MCPServers[0]; mcp.Name != "tools" || mcp.URL != "http://aries-sandbox-abc123:9000/mcp" || mcp.Transport != "sse" || mcp.TimeoutSeconds != 30 {
		t.Fatalf("MCP server = %+v", mcp)
	}
	if !objects["networkpolicy/aries-sandbox-abc123"] || !objects["service/aries-sandbox-abc123"] {
		t.Fatalf("objects = %v", objects)
	}
	policy, service := manifests[1]["spec"].(map[string]any), manifests[2]["spec"].(map[string]any)
	ingress := policy["ingress"].([]any)
	rule := ingress[0].(map[string]any)
	from := rule["from"].([]any)[0].(map[string]any)["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
	admitted := rule["ports"].([]any)
	port := admitted[0].(map[string]any)
	if len(ingress) != 1 || from[componentLabel] != "harness" || from[managedByLabel] != "aries" || len(admitted) != 2 || port["port"] != float64(8888) || port["protocol"] != "TCP" || admitted[1].(map[string]any)["port"] != float64(9000) {
		t.Fatalf("ingress = %v", ingress)
	}
	if selector := service["selector"].(map[string]any); len(selector) != 1 || selector[runtimeLabel] != "aries-sandbox-abc123" {
		t.Fatalf("service selector = %v", selector)
	}
	if target := service["ports"].([]any)[0].(map[string]any); len(service["ports"].([]any)) != 2 || target["port"] != float64(8888) || target["targetPort"] != float64(8888) {
		t.Fatalf("service ports = %v", service["ports"])
	}
	if err := task.Stop(context.Background()); err != nil || objects["service/aries-sandbox-abc123"] || objects["networkpolicy/aries-sandbox-abc123"] {
		t.Fatalf("Stop = %v, objects = %v", err, objects)
	}
	// Without a declared service, nothing is admitted and no Service exists.
	manifests = nil
	plain := run.NewTaskEnvironment()
	if connectivity, err := plain.Start(context.Background(), taskRequest(true)); err != nil || connectivity.SearchURL != "" {
		t.Fatalf("plain Start = %+v, %v", connectivity, err)
	}
	if _, admitted := manifests[0]["spec"].(map[string]any)["ingress"]; admitted || len(manifests) != 1 {
		t.Fatalf("plain task created %d objects", len(manifests))
	}
}

func TestValidateBridgeTarget(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	request := testRequest()
	pod := podJSON(t, manager, request, nil)
	fake.respond = func(_ context.Context, _ []string, _ []byte, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, pod)
		return nil
	}
	target := core.BridgeTarget{SandboxID: request.Name, RuntimeID: request.Name, RunID: "run-1", TaskID: "task-1", Backend: "kubernetes", Workdir: "/app"}
	if err := manager.ValidateBridgeTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*core.BridgeTarget){
		"backend": func(d *core.BridgeTarget) { d.Backend = "docker" },
		"run":     func(d *core.BridgeTarget) { d.RunID = "run-2" },
		"task":    func(d *core.BridgeTarget) { d.TaskID = "task-2" },
		"runtime": func(d *core.BridgeTarget) { d.RuntimeID = "aries-sandbox-other" },
	} {
		other := target
		mutate(&other)
		if err := manager.ValidateBridgeTarget(context.Background(), other); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A harness pod is never a valid execution target.
	harness := testRequest()
	harness.Labels["aries.component"] = "harness"
	pod = podJSON(t, manager, harness, nil)
	if err := manager.ValidateBridgeTarget(context.Background(), target); err == nil {
		t.Fatal("harness pod accepted as a sandbox")
	}
}

// fakePrometheus serves instant queries from per-pod CPU seconds, memory bytes
// and scrape timestamps.
type fakePrometheus struct {
	mu      sync.Mutex
	cpu     map[string]float64
	memory  map[string]float64
	scraped map[string]float64
	queries int
	status  int
}

func (f *fakePrometheus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if f.status != 0 {
		http.Error(w, "unavailable", f.status)
		return
	}
	expression := r.URL.Query().Get("query")
	if r.URL.Path != "/api/v1/query" || !strings.Contains(expression, `{container_label_io_kubernetes_pod_namespace="aries",container_label_io_kubernetes_container_name!="",container_label_io_kubernetes_container_name!="POD"}`) ||
		!strings.Contains(expression, "by (container_label_io_kubernetes_pod_name)") {
		http.Error(w, "unexpected query "+r.URL.String(), http.StatusBadRequest)
		return
	}
	values := f.cpu
	switch {
	case strings.Contains(expression, "timestamp("):
		values = f.scraped
	case strings.Contains(expression, "container_memory_working_set_bytes"):
		values = f.memory
	}
	var result []map[string]any
	for pod, value := range values {
		result = append(result, map[string]any{"metric": map[string]string{"container_label_io_kubernetes_pod_name": pod}, "value": []any{1.0, strconv.FormatFloat(value, 'f', -1, 64)}})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
}

func TestResourceSourceReadsPodCountersFromPrometheus(t *testing.T) {
	manager, _ := newTestManager(t, Options{})
	sandbox := testRequest()
	bridge := testRequest()
	bridge.Name = "aries-bridge-1"
	bridge.MemoryMB = nil
	bridge.Labels = map[string]string{"aries.managed": "true", "aries.kind": "tool-bridge", "aries.component": "bridge", "aries.run": "run-1"}
	other := testRequest()
	other.Name = "aries-sandbox-other"
	other.Labels["aries.run"] = "run-2"
	list := `{"items":[` + podJSON(t, manager, sandbox, nil) + `,` + podJSON(t, manager, bridge, nil) + `,` + podJSON(t, manager, other, nil) + `]}`
	listings := 0
	kubectl := &fakeKubectl{respond: func(_ context.Context, _ []string, _ []byte, stdout, _ io.Writer) error {
		listings++
		_, _ = io.WriteString(stdout, list)
		return nil
	}}
	prometheus := &fakePrometheus{
		cpu:     map[string]float64{"aries-sandbox-abc123": 1.5, "aries-bridge-1": 0.07, "aries-sandbox-other": 9, "unrelated-pod": 3},
		memory:  map[string]float64{"aries-sandbox-abc123": 4096, "aries-bridge-1": 10, "aries-sandbox-other": 1, "unrelated-pod": 1},
		scraped: map[string]float64{"aries-sandbox-abc123": 1000.5, "aries-bridge-1": 1000.5, "aries-sandbox-other": 1000.5, "unrelated-pod": 1000.5},
	}
	server := httptest.NewServer(prometheus)
	defer server.Close()

	task, err := NewResourceSource(ResourceOptions{RunID: "run-1", TaskIDs: []string{"task-1"}, PrometheusURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	task.kubectl = kubectl
	readings, err := task.Sample(context.Background())
	if err != nil || len(readings) != 1 {
		t.Fatalf("task readings = %+v, %v", readings, err)
	}
	r := readings[0]
	if r.Scope != "task" || r.RunID != "run-1" || r.TaskID != "task-1" || r.Component != "sandbox" || r.RuntimeID != "aries-sandbox-abc123" ||
		r.CPUUsageNanoseconds != 1_500_000_000 || r.MemoryUsageBytes != 4096 || r.MemoryLimitBytes != 512<<20 || !r.ObservedAt.Equal(time.Unix(1000, 500_000_000)) {
		t.Fatalf("task reading = %+v", r)
	}
	// Prometheus serves the same scrape until cAdvisor samples again: that is
	// not a new sample, and known pods are not listed again.
	if repeated, err := task.Sample(context.Background()); err != nil || len(repeated) != 0 {
		t.Fatalf("repeated readings = %+v, %v", repeated, err)
	}
	prometheus.mu.Lock()
	prometheus.cpu["aries-sandbox-abc123"], prometheus.scraped["aries-sandbox-abc123"] = 2.5, 1010.5
	prometheus.mu.Unlock()
	advanced, err := task.Sample(context.Background())
	if err != nil || len(advanced) != 1 || advanced[0].CPUUsageNanoseconds != 2_500_000_000 {
		t.Fatalf("advanced readings = %+v, %v", advanced, err)
	}
	if listings != 1 {
		t.Fatalf("pod listings = %d, want one: identities are cached", listings)
	}

	run, err := NewResourceSource(ResourceOptions{Scope: "run", RunID: "run-1", PrometheusURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	run.kubectl = kubectl
	readings, err = run.Sample(context.Background())
	if err != nil || len(readings) != 1 {
		t.Fatalf("run readings = %+v, %v", readings, err)
	}
	// A pod without a memory limit reports none rather than a guess.
	if r := readings[0]; r.Scope != "run" || r.TaskID != "" || r.Component != "bridge" || r.RuntimeID != "aries-bridge-1" || r.MemoryLimitBytes != 0 {
		t.Fatalf("run reading = %+v", r)
	}
}

func TestResourceSourceReportsPrometheusAndDiscoveryFailures(t *testing.T) {
	prometheus := &fakePrometheus{cpu: map[string]float64{"aries-sandbox-abc123": 1}, memory: map[string]float64{}, scraped: map[string]float64{}}
	server := httptest.NewServer(prometheus)
	defer server.Close()
	source, err := NewResourceSource(ResourceOptions{RunID: "run-1", TaskIDs: []string{"task-1"}, PrometheusURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	source.kubectl = &fakeKubectl{respond: func(context.Context, []string, []byte, io.Writer, io.Writer) error {
		return exitError(1)
	}}
	// A few failed pod listings are missing samples, not the end of telemetry.
	for attempt := 1; attempt <= discoveryFailureBudget; attempt++ {
		if readings, err := source.Sample(context.Background()); err != nil || len(readings) != 0 {
			t.Fatalf("attempt %d = %+v, %v", attempt, readings, err)
		}
	}
	if _, err := source.Sample(context.Background()); err == nil {
		t.Fatal("persistent discovery failure never surfaced")
	}
	prometheus.mu.Lock()
	prometheus.status = http.StatusServiceUnavailable
	prometheus.mu.Unlock()
	if _, err := source.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Prometheus failure = %v", err)
	}
	for _, address := range []string{"", "prometheus:9090", "ftp://prometheus", "http://user@prometheus:9090"} {
		if _, err := NewResourceSource(ResourceOptions{RunID: "run-1", TaskIDs: []string{"task-1"}, PrometheusURL: address}); err == nil {
			t.Errorf("address %q accepted", address)
		}
	}
	if _, err := NewResourceSource(ResourceOptions{Scope: "run", RunID: "run-1", TaskIDs: []string{"task-1"}, PrometheusURL: server.URL}); err == nil {
		t.Fatal("run scope accepted task IDs")
	}
}

func TestResourceSourceReportsAMissingCollectorButNotAnEmptyNamespace(t *testing.T) {
	prometheus := &fakePrometheus{cpu: map[string]float64{}, memory: map[string]float64{}, scraped: map[string]float64{}}
	server := httptest.NewServer(prometheus)
	defer server.Close()
	source, err := NewResourceSource(ResourceOptions{RunID: "run-1", TaskIDs: []string{"task-1"}, PrometheusURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	source.kubectl = &fakeKubectl{respond: func(_ context.Context, _ []string, _ []byte, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, `{"items":[]}`)
		return nil
	}}
	// No series at all: the chart's cAdvisor is not being scraped.
	if _, err := source.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "cadvisor.enabled") {
		t.Fatalf("missing collector = %v", err)
	}
	// Once its own pods have been seen, an empty answer is only a quiet moment.
	prometheus.mu.Lock()
	prometheus.cpu["aries-cadvisor-x1"], prometheus.memory["aries-cadvisor-x1"], prometheus.scraped["aries-cadvisor-x1"] = 1, 1, 1000
	prometheus.mu.Unlock()
	if readings, err := source.Sample(context.Background()); err != nil || len(readings) != 0 {
		t.Fatalf("collector only = %+v, %v", readings, err)
	}
	prometheus.mu.Lock()
	prometheus.cpu, prometheus.memory, prometheus.scraped = map[string]float64{}, map[string]float64{}, map[string]float64{}
	prometheus.mu.Unlock()
	if readings, err := source.Sample(context.Background()); err != nil || len(readings) != 0 {
		t.Fatalf("empty after collector seen = %+v, %v", readings, err)
	}
}
