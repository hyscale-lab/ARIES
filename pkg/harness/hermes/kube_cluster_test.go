//go:build kubecluster

package hermes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// TestKubeClusterStartReadyStop runs the Kubernetes harness against a real
// cluster: create the pod, verify what admission produced, stage the runtime
// over `kubectl exec`, wait for `hermes --version` to answer as the image's
// unprivileged user, then delete and confirm absence.
//
// It stages a dummy model key and never calls Run, so it needs neither a model
// API key nor a bridge. It does create a pod and pull the Hermes image on the
// target node, so it only runs when asked:
//
//	KUBECONFIG=setup/kubeconfig ARIES_KUBE_IT_OUTPUT=/private/tmp/aries-kube-it \
//	  ARIES_KUBE_IT_NODE_ROLE=harness go test -tags kubecluster \
//	  -run TestKubeClusterStartReadyStop ./pkg/harness/hermes/
//
// ARIES_KUBE_IT_OUTPUT must be a path with no symbolic link in it; on macOS use
// /private/tmp rather than /tmp or $TMPDIR.
func TestKubeClusterStartReadyStop(t *testing.T) {
	outputDir := os.Getenv("ARIES_KUBE_IT_OUTPUT")
	if outputDir == "" {
		t.Skip("set ARIES_KUBE_IT_OUTPUT to run against a real cluster")
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(outputDir, "id_ed25519")
	if err := os.WriteFile(identity, []byte("dummy identity for readiness only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(identity, 0o600)

	manager, err := NewKube(KubeOptions{
		Image: testHermesImage, OutputDir: filepath.Join(outputDir, "runs"),
		NodeRole:     os.Getenv("ARIES_KUBE_IT_NODE_ROLE"),
		StartTimeout: 10 * time.Minute, CleanupTimeout: time.Minute,
		APIKeyLookup: func(string) ([]byte, bool) { return []byte("sk-kube-cluster-it-dummy"), true },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := core.HarnessRequest{
		RunID: "kube-it", TaskID: "fix-git-" + time.Now().UTC().Format("150405"), Model: validModel(),
		Endpoint: core.ToolEndpoint{
			Protocol: "ssh", Address: "127.0.0.1:2222", Username: "aries", Network: "aries/kube-it",
			IdentityFile: identityContainerFS, IdentitySourceFile: identity,
		},
	}
	ctx := context.Background()
	defer func() {
		if err := manager.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	}()

	started := time.Now()
	if err := manager.Start(ctx, request); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Logf("pod %s ready in %s", manager.active.podName, time.Since(started).Round(time.Second))

	kubectl := execKubectl{path: defaultKubectl}
	node, _, err := kubectl.Run(ctx, nil, "get", "pod", "-n", manager.namespace, manager.active.podName, "-o", "jsonpath={.spec.nodeName}")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("scheduled on %s", strings.TrimSpace(string(node)))

	// The staged secrets must belong to the unprivileged hermes user and stay
	// private, exactly as on Docker.
	for _, path := range []string{modelKeyPath, identityContainerFS, configContainerPath} {
		out, _, err := kubectl.Run(ctx, nil, "exec", "-n", manager.namespace, manager.active.podName, "-c", kubeContainerName,
			"--", "stat", "-c", "%u:%g %a", path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := strings.TrimSpace(string(out)); got != "10000:10000 600" {
			t.Errorf("%s is %q, want 10000:10000 600", path, got)
		}
	}

	pod := manager.active.podName
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	out, _, err := kubectl.Run(ctx, nil, "get", "pod", "-n", "aries", pod, "--ignore-not-found", "-o", "name")
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("pod %s still present after Stop: %q %v", pod, out, err)
	}
}
