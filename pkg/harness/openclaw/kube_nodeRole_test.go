package openclaw

import (
	"encoding/json"
	"testing"
)

func decodePodSpec(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var pod map[string]any
	if err := json.Unmarshal(raw, &pod); err != nil {
		t.Fatalf("unmarshal pod manifest: %v", err)
	}
	spec, ok := pod["spec"].(map[string]any)
	if !ok {
		t.Fatalf("pod has no spec object: %v", pod)
	}
	return spec
}

func TestPodManifestUnpinnedWithoutNodeRole(t *testing.T) {
	session := &kubeSession{session: &session{attemptID: "abc123"}, podName: "aries-openclaw-abc123"}
	spec := decodePodSpec(t, podManifest(session, "aries", "ghcr.io/openclaw/openclaw:2026.7.1", ""))

	if _, ok := spec["nodeSelector"]; ok {
		t.Error("nodeSelector must be absent when no node role is configured")
	}
	if _, ok := spec["tolerations"]; ok {
		t.Error("tolerations must be absent when no node role is configured")
	}
}

// Every node in a role-partitioned cluster carries a taint, so an agent pod
// without a toleration is unschedulable everywhere. It needs the selector to
// reach the harness pool and the toleration to be admitted onto it.
func TestPodManifestPinsAndToleratesNodeRole(t *testing.T) {
	session := &kubeSession{session: &session{attemptID: "abc123"}, podName: "aries-openclaw-abc123"}
	spec := decodePodSpec(t, podManifest(session, "aries", "ghcr.io/openclaw/openclaw:2026.7.1", "harness"))

	selector, ok := spec["nodeSelector"].(map[string]any)
	if !ok {
		t.Fatalf("nodeSelector missing or wrong type: %T", spec["nodeSelector"])
	}
	if got := selector[nodeRoleLabel]; got != "harness" {
		t.Errorf("nodeSelector[%s] = %v, want %q", nodeRoleLabel, got, "harness")
	}

	tolerations, ok := spec["tolerations"].([]any)
	if !ok || len(tolerations) != 1 {
		t.Fatalf("want exactly one toleration, got %v", spec["tolerations"])
	}
	toleration := tolerations[0].(map[string]any)
	for field, want := range map[string]string{
		"key": nodeRoleLabel, "operator": "Equal", "value": "harness", "effect": "NoSchedule",
	} {
		if got := toleration[field]; got != want {
			t.Errorf("toleration[%q] = %v, want %q", field, got, want)
		}
	}
}
