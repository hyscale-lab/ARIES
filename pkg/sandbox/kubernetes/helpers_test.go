package kubernetes

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPodSpecUnpinnedWithoutNodeRole(t *testing.T) {
	spec := podSpec(map[string]any{"name": "sandbox"}, "")

	if _, ok := spec["nodeSelector"]; ok {
		t.Error("nodeSelector must be absent when no node role is configured")
	}
	if _, ok := spec["tolerations"]; ok {
		t.Error("tolerations must be absent when no node role is configured")
	}
}

// A pinned pod needs both halves: the selector pulls it onto the labelled node,
// the toleration gets it past that node's NoSchedule taint. Either one alone
// leaves the pod unschedulable or free to land elsewhere.
func TestPodSpecPinsAndToleratesNodeRole(t *testing.T) {
	spec := podSpec(map[string]any{"name": "sandbox"}, "sandbox")

	selector, ok := spec["nodeSelector"].(map[string]string)
	if !ok {
		t.Fatalf("nodeSelector missing or wrong type: %T", spec["nodeSelector"])
	}
	if got := selector[nodeRoleLabel]; got != "sandbox" {
		t.Errorf("nodeSelector[%s] = %q, want %q", nodeRoleLabel, got, "sandbox")
	}

	tolerations, ok := spec["tolerations"].([]any)
	if !ok || len(tolerations) != 1 {
		t.Fatalf("want exactly one toleration, got %v", spec["tolerations"])
	}
	toleration, ok := tolerations[0].(map[string]any)
	if !ok {
		t.Fatalf("toleration has wrong type: %T", tolerations[0])
	}
	for field, want := range map[string]string{
		"key": nodeRoleLabel, "operator": "Equal", "value": "sandbox", "effect": "NoSchedule",
	} {
		if got := toleration[field]; got != want {
			t.Errorf("toleration[%q] = %v, want %q", field, got, want)
		}
	}
}

func TestPodSpecMarshalsToValidPodJSON(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"spec": podSpec(map[string]any{"name": "sandbox"}, "sandbox")})
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	for _, want := range []string{`"nodeSelector":{"aries.dev/role":"sandbox"}`, `"effect":"NoSchedule"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("rendered pod is missing %s\ngot: %s", want, raw)
		}
	}
}

func TestValidateNodeRole(t *testing.T) {
	valid := []string{"", "sandbox", "harness", "gpu-pool", "pool_1", "a.b", "a", strings.Repeat("x", 63)}
	for _, role := range valid {
		if err := validateNodeRole(role); err != nil {
			t.Errorf("validateNodeRole(%q) = %v, want nil", role, err)
		}
	}

	invalid := []string{
		"has space", "slash/role", "aries.dev/role", "-leading", "trailing-",
		".dot", "under_", strings.Repeat("x", 64),
	}
	for _, role := range invalid {
		if err := validateNodeRole(role); err == nil {
			t.Errorf("validateNodeRole(%q) = nil, want an error", role)
		}
	}
}
