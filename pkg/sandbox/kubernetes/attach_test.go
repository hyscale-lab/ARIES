package kubernetes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSandboxID = "0123456789abcdef"

func attachablePod(mutate func(map[string]any)) []byte {
	pod := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{
				"app.kubernetes.io/managed-by": "aries", "app.kubernetes.io/component": "sandbox",
				sandboxIDLabel: testSandboxID,
			},
			"annotations": map[string]any{"aries.dev/run-id": "run-1", "aries.dev/task-id": "task-1"},
		},
		"status": map[string]any{"phase": "Running"},
	}
	if mutate != nil {
		mutate(pod)
	}
	raw, _ := json.Marshal(pod)
	return raw
}

func attachTarget() *Sandbox {
	return &Sandbox{namespace: "aries", podName: "aries-task-" + testSandboxID, sandboxID: testSandboxID, runID: "run-1", taskID: "task-1"}
}

func TestCheckAttachableAcceptsTheNamedSandbox(t *testing.T) {
	if err := checkAttachable(attachablePod(nil), attachTarget()); err != nil {
		t.Fatal(err)
	}
}

// Each of these is a pod the bridge must never exec into: the request names
// the pod, so these checks are what stops a wrong request from reaching the
// harness pod or another task's sandbox.
func TestCheckAttachableRefusesOtherPods(t *testing.T) {
	labels := func(pod map[string]any) map[string]any {
		return pod["metadata"].(map[string]any)["labels"].(map[string]any)
	}
	annotations := func(pod map[string]any) map[string]any {
		return pod["metadata"].(map[string]any)["annotations"].(map[string]any)
	}
	cases := map[string]func(map[string]any){
		"harness pod":   func(pod map[string]any) { labels(pod)["app.kubernetes.io/component"] = "harness" },
		"not managed":   func(pod map[string]any) { delete(labels(pod), "app.kubernetes.io/managed-by") },
		"other sandbox": func(pod map[string]any) { labels(pod)[sandboxIDLabel] = "fedcba9876543210" },
		"other run":     func(pod map[string]any) { annotations(pod)["aries.dev/run-id"] = "run-2" },
		"other task":    func(pod map[string]any) { annotations(pod)["aries.dev/task-id"] = "task-2" },
		"being deleted": func(pod map[string]any) {
			pod["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-27T00:00:00Z"
		},
		"not running":         func(pod map[string]any) { pod["status"] = map[string]any{"phase": "Pending"} },
		"finished":            func(pod map[string]any) { pod["status"] = map[string]any{"phase": "Succeeded"} },
		"no labels at all":    func(pod map[string]any) { pod["metadata"].(map[string]any)["labels"] = map[string]any{} },
		"no annotations":      func(pod map[string]any) { pod["metadata"].(map[string]any)["annotations"] = nil },
		"empty sandbox label": func(pod map[string]any) { labels(pod)[sandboxIDLabel] = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkAttachable(attachablePod(mutate), attachTarget()); err == nil {
				t.Fatal("attached to a pod it must refuse")
			}
		})
	}
}

// A request that fails these checks must be refused before kubectl runs, so
// the kubectl path here does not exist.
func TestAttachRejectsMalformedRequestsWithoutContactingTheCluster(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent-kubectl")
	valid := AttachOptions{
		Namespace: "aries", PodName: "aries-task-" + testSandboxID, SandboxID: testSandboxID,
		Workdir: "/app", RunID: "run-1", TaskID: "task-1", KubectlPath: absent,
	}
	cases := map[string]func(*AttachOptions){
		"pod not derived from ID": func(o *AttachOptions) { o.PodName = "aries-hermes-abc" },
		"short ID":                func(o *AttachOptions) { o.SandboxID, o.PodName = "abc", "aries-task-abc" },
		"uppercase ID":            func(o *AttachOptions) { o.SandboxID = strings.ToUpper(testSandboxID) },
		"bad run":                 func(o *AttachOptions) { o.RunID = "run/1" },
		"bad task":                func(o *AttachOptions) { o.TaskID = "" },
		"relative workdir":        func(o *AttachOptions) { o.Workdir = "app" },
		"unclean workdir":         func(o *AttachOptions) { o.Workdir = "/app/../etc" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			options := valid
			mutate(&options)
			_, err := Attach(context.Background(), options)
			if err == nil {
				t.Fatal("accepted a malformed attach request")
			}
			if strings.Contains(err.Error(), "read kubernetes task pod") {
				t.Fatalf("contacted the cluster before validating: %v", err)
			}
		})
	}
}

// With a well-formed request, Attach reads the pod and returns a handle that
// names it, and that handle cannot be stopped through any Manager.
func TestAttachReadsThePodAndReturnsAnUnstoppableHandle(t *testing.T) {
	dir := t.TempDir()
	podJSON := filepath.Join(dir, "pod.json")
	if err := os.WriteFile(podJSON, attachablePod(nil), 0o600); err != nil {
		t.Fatal(err)
	}
	kubectl := filepath.Join(dir, "kubectl")
	script := "#!/bin/sh\n[ \"$1 $2 $3 $4 $5\" = \"get pod -n aries aries-task-" + testSandboxID + "\" ] || exit 9\ncat " + podJSON + "\n"
	if err := os.WriteFile(kubectl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sandbox, err := Attach(context.Background(), AttachOptions{
		Namespace: "aries", PodName: "aries-task-" + testSandboxID, SandboxID: testSandboxID,
		Workdir: "/app", RunID: "run-1", TaskID: "task-1", KubectlPath: kubectl,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ContainerName() != "aries-task-"+testSandboxID || sandbox.Namespace() != "aries" || sandbox.SandboxID() != testSandboxID || sandbox.Workdir() != "/app" {
		t.Fatalf("handle names the wrong pod: %+v", sandbox)
	}
	manager, err := New(Options{OutputDir: t.TempDir(), KubectlPath: kubectl})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), sandbox); err == nil {
		t.Fatal("a manager stopped a sandbox it did not start")
	}
}
