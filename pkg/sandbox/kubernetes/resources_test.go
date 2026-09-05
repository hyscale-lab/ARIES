package kubernetes

import (
	"encoding/json"
	"testing"
	"time"
)

// summaryFixture mirrors the shape the kubelet actually returns, captured from
// /api/v1/nodes/<node>/proxy/stats/summary on a v1.37 cluster. The psi blocks
// and the extra memory fields are kept deliberately: they are present in the
// real payload, and their presence is what proves the decoder ignores what it
// does not need rather than depending on an exact schema.
const summaryFixture = `{
  "node": {"nodeName": "node3"},
  "pods": [
    {
      "podRef": {"name": "aries-task-abc123", "namespace": "aries"},
      "cpu": {
        "time": "2026-09-05T06:15:37Z",
        "usageNanoCores": 9020628,
        "usageCoreNanoSeconds": 117225896000,
        "psi": {"full": {"total": 133711}, "some": {"total": 133751}}
      },
      "memory": {
        "time": "2026-09-05T06:15:37Z",
        "usageBytes": 21311488,
        "workingSetBytes": 20963328,
        "rssBytes": 14872576,
        "availableBytes": 1052672
      }
    },
    {
      "podRef": {"name": "other-pod", "namespace": "aries"},
      "cpu": {"time": "2026-09-05T06:15:37Z", "usageCoreNanoSeconds": 999},
      "memory": {"time": "2026-09-05T06:15:37Z", "workingSetBytes": 999}
    }
  ]
}`

func decodeSummary(t *testing.T) *nodeStats {
	t.Helper()
	var stats nodeStats
	if err := json.Unmarshal([]byte(summaryFixture), &stats); err != nil {
		t.Fatalf("decode summary fixture: %v", err)
	}
	return &stats
}

// usageCoreNanoSeconds is a cumulative counter, and pkg/monitor differentiates
// it to get a rate. If this were ever wired to an instantaneous field
// (usageNanoCores, or metrics-server's millicores) the numbers would still look
// plausible but every rate downstream would be wrong, so the exact field
// matters more than the magnitude.
func TestReadingUsesCumulativeCPUCounter(t *testing.T) {
	target := resourceTarget{
		podName: "aries-task-abc123", namespace: "aries",
		node: "node3", taskID: "fix-git-002", component: "sandbox",
		limit: 512 << 20,
	}

	reading, ok := decodeSummary(t).reading(target)
	if !ok {
		t.Fatal("expected a reading for a pod present in the summary")
	}
	if reading.CPUUsageNanoseconds != 117225896000 {
		t.Errorf("CPUUsageNanoseconds = %d, want 117225896000 (usageCoreNanoSeconds, not usageNanoCores)",
			reading.CPUUsageNanoseconds)
	}
	if reading.MemoryUsageBytes != 20963328 {
		t.Errorf("MemoryUsageBytes = %d, want 20963328 (workingSetBytes)", reading.MemoryUsageBytes)
	}
	if reading.MemoryLimitBytes != 512<<20 {
		t.Errorf("MemoryLimitBytes = %d, want the pod spec limit 536870912", reading.MemoryLimitBytes)
	}
	if reading.TaskID != "fix-git-002" || reading.Component != "sandbox" {
		t.Errorf("identity = %s/%s, want fix-git-002/sandbox", reading.TaskID, reading.Component)
	}
	if reading.RuntimeID != "aries-task-abc123" {
		t.Errorf("RuntimeID = %q, want the bare pod name", reading.RuntimeID)
	}
	if reading.ObservedAt.IsZero() {
		t.Error("ObservedAt must carry the kubelet's observation time")
	}
}

// With no limit in the pod spec, the kubelet's headroom figure is the only way
// to recover the ceiling the pod is actually measured against.
func TestReadingRecoversLimitFromHeadroomWhenUnset(t *testing.T) {
	target := resourceTarget{
		podName: "aries-task-abc123", namespace: "aries",
		taskID: "fix-git-002", component: "sandbox", limit: 0,
	}

	reading, ok := decodeSummary(t).reading(target)
	if !ok {
		t.Fatal("expected a reading")
	}
	if want := uint64(20963328 + 1052672); reading.MemoryLimitBytes != want {
		t.Errorf("MemoryLimitBytes = %d, want %d (workingSet + available)", reading.MemoryLimitBytes, want)
	}
}

// A pod that ended between discovery and the summary read must yield no reading
// rather than a zero-valued one. A zero CPU counter would read downstream as the
// counter resetting, which pkg/monitor would see as a negative delta.
func TestReadingAbsentPodYieldsNothing(t *testing.T) {
	target := resourceTarget{podName: "aries-task-gone", namespace: "aries", taskID: "fix-git-002", component: "sandbox"}

	if _, ok := decodeSummary(t).reading(target); ok {
		t.Error("a pod missing from the summary must not produce a reading")
	}
}

// Namespace is part of the join key: two namespaces can hold pods with the same
// name, and matching on name alone would attribute another namespace's usage to
// this task.
func TestReadingMatchesOnNamespaceToo(t *testing.T) {
	target := resourceTarget{podName: "aries-task-abc123", namespace: "other", taskID: "fix-git-002", component: "sandbox"}

	if _, ok := decodeSummary(t).reading(target); ok {
		t.Error("a same-named pod in a different namespace must not match")
	}
}

func TestParseMemoryQuantity(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"512Mi", 512 << 20},
		{"2Gi", 2 << 30},
		{"1024Ki", 1024 << 10},
		{"1000000", 1000000},
		{"", 0},
		// Unparseable forms must yield 0 so the caller falls back to the
		// kubelet's headroom rather than recording a wrong ceiling.
		{"1.5Gi", 0},
		{"garbage", 0},
	}
	for _, test := range cases {
		if got := parseMemoryQuantity(test.in); got != test.want {
			t.Errorf("parseMemoryQuantity(%q) = %d, want %d", test.in, got, test.want)
		}
	}
}

func TestResourceComponentRejectsUnknown(t *testing.T) {
	for _, name := range []string{"sandbox", "harness"} {
		if got, ok := resourceComponent(name); !ok || got != name {
			t.Errorf("resourceComponent(%q) = %q,%v; want it supported", name, got, ok)
		}
	}
	if _, ok := resourceComponent("aries"); ok {
		t.Error("the ARIES pod itself must not be sampled as a task component")
	}
}

func TestNewResourceSourceRequiresIdentity(t *testing.T) {
	if _, err := NewResourceSource(ResourceOptions{TaskIDs: []string{"t"}}); err == nil {
		t.Error("a missing run ID must be rejected")
	}
	if _, err := NewResourceSource(ResourceOptions{RunID: "r"}); err == nil {
		t.Error("an empty task list must be rejected")
	}
	if _, err := NewResourceSource(ResourceOptions{RunID: "r", TaskIDs: []string{"t", "t"}}); err == nil {
		t.Error("a repeated task ID must be rejected")
	}
	source, err := NewResourceSource(ResourceOptions{RunID: "r", TaskIDs: []string{"t"}})
	if err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	if source.namespace != defaultNamespace {
		t.Errorf("namespace = %q, want the %q default", source.namespace, defaultNamespace)
	}
}

// pkg/monitor uses the runtime ID as a path component and rejects anything
// outside [A-Za-z0-9-_.], so a namespaced "<ns>/<pod>" ID makes the recorder
// fail with zero samples and an error buried in monitor/index.json — the run
// still succeeds, so it is easy to miss. The Docker backend never trips this
// because container IDs are hex, which is exactly why it needs asserting here.
func TestReadingIdentifiersAreMonitorSafe(t *testing.T) {
	target := resourceTarget{
		podName: "aries-task-abc123", namespace: "aries",
		taskID: "fix-git-002", component: "sandbox",
	}

	reading, ok := decodeSummary(t).reading(target)
	if !ok {
		t.Fatal("expected a reading")
	}
	for _, field := range []struct{ name, value string }{
		{"RuntimeID", reading.RuntimeID},
		{"RuntimeName", reading.RuntimeName},
	} {
		if field.value == "" {
			t.Errorf("%s must not be empty", field.name)
		}
		for index, r := range field.value {
			safe := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
				r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.'
			if !safe {
				t.Errorf("%s = %q contains %q at %d, which pkg/monitor rejects as an unsafe path character",
					field.name, field.value, r, index)
			}
			if index == 0 && (r == '-' || r == '.') {
				t.Errorf("%s = %q may not begin with %q", field.name, field.value, r)
			}
		}
	}
}

// The kubelet does not compute stats on demand; the Summary API serves a cache
// refreshed roughly every 10-20s. Polling it at the monitor's 1s interval
// returns the same observation, timestamp included, many times over — and
// pkg/monitor divides by the gap between consecutive observation times, so a
// repeat is a division by zero that fails the whole monitor for that task.
// Measured on a v1.37 kubelet: identical timestamps across polls 2s apart, then
// an 18s jump.
func TestRepeatedKubeletObservationsAreSuppressed(t *testing.T) {
	source, err := NewResourceSource(ResourceOptions{RunID: "run", TaskIDs: []string{"fix-git-001"}})
	if err != nil {
		t.Fatalf("NewResourceSource: %v", err)
	}
	first := time.Date(2026, 9, 5, 7, 34, 46, 0, time.UTC)

	if !source.observationIsNew("aries-task-abc", first) {
		t.Fatal("the first observation for a runtime must be accepted")
	}
	if source.observationIsNew("aries-task-abc", first) {
		t.Error("the same observation time must be suppressed, not re-emitted")
	}
	if source.observationIsNew("aries-task-abc", first.Add(-time.Second)) {
		t.Error("an older observation time must be suppressed")
	}
	if !source.observationIsNew("aries-task-abc", first.Add(18*time.Second)) {
		t.Error("an advanced observation time must be accepted")
	}
	// Runtimes are tracked independently: one pod's cadence must not suppress
	// another's first reading.
	if !source.observationIsNew("aries-task-xyz", first) {
		t.Error("a different runtime must be tracked separately")
	}
}
