package kubernetes

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// fakePool answers kubectl as agent-sandbox's controller would: a claim binds
// a pooled pod named by the pool, and deleting the claim removes it.
type fakePool struct {
	mu      sync.Mutex
	t       *testing.T
	manager *Manager
	request deployment.Request
	objects map[string]bool // resource/name present
	podJSON string
	patched map[string]any
	created []map[string]any
}

const pooledPod = "aries-pool-0123-x7k2p"

func (f *fakePool) respond(_ context.Context, args []string, stdin []byte, stdout, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "create":
		manifest := decodeManifest(f.t, stdin)
		f.created = append(f.created, manifest)
		name := manifest["metadata"].(map[string]any)["name"].(string)
		f.objects[manifest["kind"].(string)+"/"+name] = true
		if manifest["kind"] == "SandboxClaim" {
			f.objects["pod/"+pooledPod], f.objects[sandboxResource+"/"+pooledPod] = true, true
		}
	case "get":
		resource, name := args[1], args[4]
		switch {
		case resource == claimResource && slices.Contains(args, "jsonpath={.status.sandbox.name}"):
			_, _ = io.WriteString(stdout, pooledPod)
		case resource == "pod" && f.objects["pod/"+name]:
			_, _ = io.WriteString(stdout, f.podJSON)
		case resource == claimResource && f.objects["SandboxClaim/"+name], resource == sandboxResource && f.objects[sandboxResource+"/"+name]:
			_, _ = io.WriteString(stdout, `{"metadata":{"name":"`+name+`"}}`)
		}
	case "patch":
		_ = json.Unmarshal([]byte(args[len(args)-1]), &f.patched)
	case "delete":
		if args[1] == claimResource && f.objects["SandboxClaim/"+args[4]] {
			delete(f.objects, "SandboxClaim/"+args[4])
			delete(f.objects, "pod/"+pooledPod)
			delete(f.objects, sandboxResource+"/"+pooledPod)
		}
	}
	return nil
}

func TestWarmPoolClaimsAPooledPodAndGivesItTheTaskIdentity(t *testing.T) {
	manager, fake := newTestManager(t, Options{AgentSandbox: true, WarmPool: 2, RuntimeClass: "gvisor"})
	request := testRequest()
	pool := &fakePool{t: t, manager: manager, request: request, objects: map[string]bool{}}
	// The pod the controller made from the template, before ARIES labels it.
	pool.podJSON = sandboxPodJSON(t, manager, request, func(m map[string]any) {
		metadata := m["metadata"].(map[string]any)
		metadata["name"] = pooledPod
		delete(metadata["labels"].(map[string]any), runtimeLabel)
		delete(metadata, "annotations")
		metadata["ownerReferences"] = []any{map[string]any{"apiVersion": sandboxAPIVersion, "kind": "Sandbox", "name": pooledPod, "controller": true}}
	})
	fake.respond = pool.respond

	id, err := manager.Create(context.Background(), request)
	if err != nil || id != pooledPod {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if len(pool.created) != 3 || pool.created[0]["kind"] != "SandboxTemplate" || pool.created[1]["kind"] != "SandboxWarmPool" || pool.created[2]["kind"] != "SandboxClaim" {
		t.Fatalf("created = %v", pool.created)
	}
	template := pool.created[0]["spec"].(map[string]any)
	// Pooled pods are denied all traffic until a task's own policy selects one.
	if template["networkPolicyManagement"] != "Managed" || mustJSON(t, template["networkPolicy"]) != `{"egress":[],"ingress":[]}` {
		t.Fatalf("template policy = %v", template)
	}
	// The template is the request's pod without the task's identity.
	podTemplate := template["podTemplate"].(map[string]any)
	labels := podTemplate["metadata"].(map[string]any)["labels"].(map[string]any)
	if _, named := labels[runtimeLabel]; named || podTemplate["metadata"].(map[string]any)["annotations"] != nil {
		t.Fatalf("template carries a task identity: %v", podTemplate["metadata"])
	}
	if podTemplate["spec"].(map[string]any)["runtimeClassName"] != "gvisor" {
		t.Fatalf("template spec = %v", podTemplate["spec"])
	}
	if replicas := pool.created[1]["spec"].(map[string]any)["replicas"]; replicas != float64(2) {
		t.Fatalf("replicas = %v", replicas)
	}
	claim := pool.created[2]
	if claim["metadata"].(map[string]any)["name"] != request.Name || claim["spec"].(map[string]any)["lifecycle"].(map[string]any)["shutdownPolicy"] != "DeleteForeground" {
		t.Fatalf("claim = %v", claim)
	}
	patchedLabels := pool.patched["metadata"].(map[string]any)["labels"].(map[string]any)
	patchedAnnotations := pool.patched["metadata"].(map[string]any)["annotations"].(map[string]any)
	if patchedLabels[runtimeLabel] != request.Name || patchedAnnotations["aries.run"] != "run-1" || patchedAnnotations["aries.task"] != "task-1" {
		t.Fatalf("patch = %v", pool.patched)
	}

	// A second task of the run reuses the template and pool.
	alreadyExists := func(_ context.Context, args []string, stdin []byte, stdout, stderr io.Writer) error {
		if args[0] == "create" && strings.Contains(string(stdin), `"kind":"Sandbox`) && !strings.Contains(string(stdin), `"kind":"SandboxClaim"`) {
			_, _ = io.WriteString(stderr, "Error from server (AlreadyExists): already exists")
			return exitError(1)
		}
		return pool.respond(context.Background(), args, stdin, stdout, stderr)
	}
	fake.respond = alreadyExists
	second := testRequest()
	second.Name = "aries-sandbox-def456"
	if _, err := manager.Create(context.Background(), second); err != nil {
		t.Fatalf("second Create = %v", err)
	}
	if got, want := pool.created[3]["spec"].(map[string]any)["warmPoolRef"].(map[string]any)["name"], pool.created[0]["metadata"].(map[string]any)["name"]; got != want {
		t.Fatalf("second task used pool %v, want %v", got, want)
	}
	fake.respond = pool.respond

	// Validate checks the label the claim gave the pod, not its name.
	labelled := strings.Replace(pool.podJSON, `"labels":{`, `"annotations":`+mustJSON(t, request.Labels)+`,"labels":{"`+runtimeLabel+`":"`+request.Name+`",`, 1)
	for name, test := range map[string]struct {
		pod string
		ok  bool
	}{
		"labelled":   {labelled, true},
		"unlabelled": {pool.podJSON, false},
		"other task": {strings.Replace(labelled, `"`+runtimeLabel+`":"`+request.Name+`"`, `"`+runtimeLabel+`":"aries-sandbox-other"`, 1), false},
	} {
		pool.podJSON = test.pod
		if err := manager.Validate(context.Background(), pooledPod, request, nil); (err == nil) != test.ok {
			t.Errorf("%s: Validate = %v", name, err)
		}
	}
	pool.podJSON = labelled

	// The bridge accepts the pooled pod as the runtime of the sandbox it is
	// labelled for.
	target := core.BridgeTarget{SandboxID: request.Name, RuntimeID: pooledPod, RunID: "run-1", TaskID: "task-1", Backend: "kubernetes"}
	if err := manager.ValidateBridgeTarget(context.Background(), target); err != nil {
		t.Fatalf("ValidateBridgeTarget = %v", err)
	}
	target.SandboxID = "aries-sandbox-other"
	if err := manager.ValidateBridgeTarget(context.Background(), target); err == nil {
		t.Fatal("pooled pod accepted for another sandbox")
	}

	// Stop deletes the claim the pod is labelled with and confirms that the
	// claim, the Sandbox and the pod are gone.
	fake.calls = nil
	if err := manager.Stop(context.Background(), pooledPod); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if remove := fake.calls[1]; remove[0] != "delete" || remove[1] != claimResource || remove[4] != request.Name || !slices.Contains(remove, "--cascade=foreground") {
		t.Fatalf("delete = %v", remove)
	}
	pool.objects["pod/"+pooledPod] = true
	if err := manager.Stop(context.Background(), pooledPod); err == nil {
		t.Fatal("Stop confirmed absence of a pod that remains")
	}
}

func TestRunEnvironmentRemovesItsWarmPools(t *testing.T) {
	manager, fake := newTestManager(t, Options{AgentSandbox: true, WarmPool: 1})
	remaining := ""
	fake.respond = func(_ context.Context, args []string, _ []byte, stdout, _ io.Writer) error {
		if args[0] == "get" && args[1] == templateResource {
			_, _ = io.WriteString(stdout, remaining)
		}
		return nil
	}
	run := manager.NewRunEnvironment("run-1")
	if _, err := run.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	if err := run.Stop(context.Background()); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	selector := runHashLabel + "=" + shortHash("run-1")
	var deleted []string
	for _, call := range fake.calls {
		if call[0] == "delete" && slices.Contains(call, selector) {
			deleted = append(deleted, call[1])
		}
	}
	if !slices.Equal(deleted, []string{poolResource, templateResource}) {
		t.Fatalf("deleted = %v (calls %v)", deleted, fake.calls)
	}

	remaining = "sandboxtemplate.extensions.agents.x-k8s.io/aries-pool-1"
	run = manager.NewRunEnvironment("run-1")
	if _, err := run.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := run.Stop(context.Background()); err == nil {
		t.Fatal("Stop confirmed removal of a template that remains")
	}
}

func TestWarmPoolNeedsAgentSandbox(t *testing.T) {
	for _, options := range []Options{{WarmPool: 1}, {AgentSandbox: true, WarmPool: -1}} {
		if _, err := New(options); err == nil {
			t.Errorf("New(%+v) accepted", options)
		}
	}
}
