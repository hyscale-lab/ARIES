package kubernetes

import (
	"encoding/json"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

func testSandbox() *Sandbox {
	return &Sandbox{
		owner:      &Manager{namespace: "aries", nodeRole: "sandbox", podCIDR: "192.168.0.0/16", serviceCIDR: "10.96.0.0/12"},
		namespace:  "aries",
		podName:    "aries-task-abc123",
		sandboxID:  "abc123",
		policyName: "aries-task-abc123",
		runID:      "20260905T041640.164745562Z-openclaw-tb2",
		taskID:     "fix-git-002",
	}
}

func decodePolicy(t *testing.T) map[string]any { return decodePolicyFor(t, false) }

func decodePolicyFor(t *testing.T, allowNetwork bool) map[string]any {
	t.Helper()
	raw, err := networkPolicyManifest(testSandbox(), allowNetwork)
	if err != nil {
		t.Fatalf("networkPolicyManifest: %v", err)
	}
	var policy map[string]any
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatalf("policy is not valid JSON: %v", err)
	}
	return policy
}

// The deny-all contract has an easy-to-get-wrong spelling: naming a direction in
// policyTypes with no corresponding rule block denies it, whereas adding an
// empty rule list would *allow* it. This asserts the denying form specifically,
// because the permitting form is also valid JSON and would pass a looser test
// while leaving every task pod fully connected.
func TestAirGappedPolicyDeniesBothDirections(t *testing.T) {
	spec, ok := decodePolicy(t)["spec"].(map[string]any)
	if !ok {
		t.Fatal("policy has no spec object")
	}

	types, ok := spec["policyTypes"].([]any)
	if !ok || len(types) != 2 {
		t.Fatalf("policyTypes must name exactly Ingress and Egress, got %v", spec["policyTypes"])
	}
	seen := map[string]bool{}
	for _, entry := range types {
		name, ok := entry.(string)
		if !ok {
			t.Fatalf("policyType %v is not a string", entry)
		}
		seen[name] = true
	}
	if !seen["Ingress"] || !seen["Egress"] {
		t.Errorf("policyTypes must contain both Ingress and Egress, got %v", types)
	}

	// The absence of these keys is what denies the traffic.
	if _, present := spec["ingress"]; present {
		t.Error("an ingress rule block must be absent; its presence would permit traffic")
	}
	if _, present := spec["egress"]; present {
		t.Error("an egress rule block must be absent; its presence would permit traffic")
	}
}

// The selector must match this one pod. A policy that selected all pods in the
// namespace would isolate the run as a whole but not tasks from each other,
// which is the property being implemented.
func TestNetworkPolicySelectsOnlyItsOwnPod(t *testing.T) {
	spec := decodePolicy(t)["spec"].(map[string]any)

	selector, ok := spec["podSelector"].(map[string]any)
	if !ok {
		t.Fatal("policy has no podSelector")
	}
	labels, ok := selector["matchLabels"].(map[string]any)
	if !ok || len(labels) == 0 {
		t.Fatalf("podSelector must match on labels, got %v; an empty selector targets every pod in the namespace", selector)
	}
	if got := labels[sandboxIDLabel]; got != "abc123" {
		t.Errorf("podSelector[%s] = %v, want abc123", sandboxIDLabel, got)
	}
}

// The policy selector is only meaningful if the pod actually carries the label
// it selects on. These two manifests are generated independently, so a rename in
// one would silently disarm isolation without this check.
func TestPodCarriesTheLabelThePolicySelects(t *testing.T) {
	sandbox := testSandbox()
	raw, err := podManifest(sandbox, sandboxRequestForTest())
	if err != nil {
		t.Fatalf("podManifest: %v", err)
	}
	var pod struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		t.Fatalf("pod is not valid JSON: %v", err)
	}
	if got := pod.Metadata.Labels[sandboxIDLabel]; got != sandbox.sandboxID {
		t.Errorf("pod label %s = %q, want %q; the NetworkPolicy selector would match nothing",
			sandboxIDLabel, got, sandbox.sandboxID)
	}
}

func TestNetworkPolicyIsNamespacedAndNamed(t *testing.T) {
	metadata, ok := decodePolicy(t)["metadata"].(map[string]any)
	if !ok {
		t.Fatal("policy has no metadata")
	}
	if got := metadata["namespace"]; got != "aries" {
		t.Errorf("namespace = %v, want aries", got)
	}
	if got := metadata["name"]; got != "aries-task-abc123" {
		t.Errorf("name = %v, want aries-task-abc123", got)
	}
}

func sandboxRequestForTest() core.SandboxRequest {
	return core.SandboxRequest{
		RunID:  "20260905T041640.164745562Z-openclaw-tb2",
		TaskID: "fix-git-002",
		Environment: core.Environment{
			Image: "ghcr.io/example/task:1.0", Workdir: "/app",
		},
	}
}

// The regression this guards against: treating AllowNetwork as deciding whether
// a policy exists rather than what it permits. Docker gives each task its own
// network, so two tasks are isolated from each other whether or not either may
// reach the internet. A flat cluster pod network gives nothing for free, so a
// network-allowed task with no policy at all can dial its concurrent peers.
func TestNetworkAllowedTaskStillGetsAPolicy(t *testing.T) {
	spec, ok := decodePolicyFor(t, true)["spec"].(map[string]any)
	if !ok {
		t.Fatal("a network-allowed task must still get a policy")
	}
	types, _ := spec["policyTypes"].([]any)
	if len(types) != 2 {
		t.Fatalf("policyTypes = %v, want both Ingress and Egress", types)
	}
	// Ingress denial is the half that isolates tasks from each other, and it
	// holds regardless of what egress permits.
	if _, present := spec["ingress"]; present {
		t.Error("ingress must stay denied even when the task may reach the internet")
	}
	if _, present := spec["egress"]; !present {
		t.Fatal("a network-allowed task needs explicit egress rules")
	}
}

// Egress must reach the internet but not the cluster. Without the except list a
// task could dial its peers, the agent pods and the API server.
func TestNetworkAllowedEgressExcludesClusterNetworks(t *testing.T) {
	spec := decodePolicyFor(t, true)["spec"].(map[string]any)
	rules := spec["egress"].([]any)

	var block map[string]any
	for _, rule := range rules {
		for _, to := range rule.(map[string]any)["to"].([]any) {
			if ip, ok := to.(map[string]any)["ipBlock"].(map[string]any); ok {
				block = ip
			}
		}
	}
	if block == nil {
		t.Fatal("no ipBlock egress rule found")
	}
	if block["cidr"] != "0.0.0.0/0" {
		t.Errorf("cidr = %v, want 0.0.0.0/0", block["cidr"])
	}
	except, ok := block["except"].([]any)
	if !ok || len(except) != 2 {
		t.Fatalf("except = %v, want the pod and service CIDRs; without them the task can reach every pod in the cluster", block["except"])
	}
}

// DNS must be allowed by selecting the CoreDNS pods, not the kube-dns Service
// IP: kube-proxy DNATs Service addresses before egress policy is evaluated, so a
// rule naming the Service address never matches and the cluster-CIDR exclusion
// then drops the resolved pod IP. That breaks name resolution while leaving raw
// IP traffic working, which is a confusing way to fail.
func TestNetworkAllowedEgressPermitsDNSByPodSelector(t *testing.T) {
	spec := decodePolicyFor(t, true)["spec"].(map[string]any)

	var found bool
	for _, rule := range spec["egress"].([]any) {
		entry := rule.(map[string]any)
		ports, hasPorts := entry["ports"].([]any)
		if !hasPorts {
			continue
		}
		for _, to := range entry["to"].([]any) {
			target := to.(map[string]any)
			if _, usesIPBlock := target["ipBlock"]; usesIPBlock {
				t.Error("DNS must be selected by pod, not by Service IP block")
			}
			if _, usesPodSelector := target["podSelector"]; usesPodSelector && len(ports) == 2 {
				found = true
			}
		}
	}
	if !found {
		t.Error("no DNS egress rule selecting the CoreDNS pods on UDP+TCP 53")
	}
}

// Isolation between tasks must not depend on the CIDRs being configured. With
// none set, egress widens but ingress stays shut, so peers remain unreachable.
func TestUnconfiguredCIDRsStillDenyIngress(t *testing.T) {
	sandbox := testSandbox()
	sandbox.owner = &Manager{namespace: "aries"}

	raw, err := networkPolicyManifest(sandbox, true)
	if err != nil {
		t.Fatalf("networkPolicyManifest: %v", err)
	}
	var policy struct {
		Spec struct {
			Ingress     []any `json:"ingress"`
			PolicyTypes []any `json:"policyTypes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(policy.Spec.Ingress) != 0 || len(policy.Spec.PolicyTypes) != 2 {
		t.Error("ingress must stay denied when no CIDRs are configured")
	}
}
