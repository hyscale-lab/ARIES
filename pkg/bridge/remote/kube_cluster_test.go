//go:build kubecluster

package remote

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/core"
	k8ssandbox "github.com/hyscale-lab/aries/pkg/sandbox/kubernetes"
	"golang.org/x/crypto/ssh"
)

// TestKubeClusterBridgePod drives the deployed aries-bridge pod the way the
// runner does, from outside the cluster: a real sandbox pod, a real grant,
// a harness-labelled pod that reaches the bridge with the runner's key and
// runs a tool call in the sandbox, then revocation and log collection. It
// also checks the bridge's NetworkPolicy turns away a pod that is not a
// harness, and that deleting the bridge pod mid-grant is treated as proven
// revocation with lost evidence.
//
// It creates pods and deletes the bridge pod, so it only runs when asked:
//
//	KUBECONFIG=~/aries/kubeconfig ARIES_KUBE_IT_OUTPUT=/tmp/aries-bridge-it \
//	  ./remote.test -test.run TestKubeClusterBridgePod -test.v
func TestKubeClusterBridgePod(t *testing.T) {
	output := os.Getenv("ARIES_KUBE_IT_OUTPUT")
	if output == "" {
		t.Skip("set ARIES_KUBE_IT_OUTPUT to run against a real cluster")
	}
	if err := os.MkdirAll(output, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runID := "bridge-it-" + time.Now().UTC().Format("150405")

	sandboxes, err := k8ssandbox.New(k8ssandbox.Options{OutputDir: filepath.Join(output, "sandbox"), Namespace: testNamespace})
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := sandboxes.Start(ctx, core.SandboxRequest{RunID: runID, TaskID: "task-1",
		Environment: core.Environment{Image: "debian:bookworm-slim", Workdir: "/root"}})
	if err != nil {
		t.Fatalf("start sandbox: %v", err)
	}
	defer func() {
		if err := sandboxes.Stop(context.Background(), sandbox); err != nil {
			t.Errorf("stop sandbox: %v", err)
		}
	}()
	sandboxPod := sandbox.(interface{ ContainerName() string }).ContainerName()

	harness := startClientPod(t, runID+"-harness", true)
	outsider := startClientPod(t, runID+"-outsider", false)

	options := Options{BridgeType: "hermes-ssh", Transport: NewKubeTransport(testNamespace, ""), OutputDir: filepath.Join(output, "runs"),
		NewCredentials: func(dir string) (Credentials, error) { return hermesssh.NewCredentials(dir) }}
	if err := Preflight(ctx, options); err != nil {
		t.Fatalf("preflight: %v", err)
	}

	// 1. A grant serves a tool call from a harness pod into the sandbox.
	client, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	endpoint, err := client.Start(ctx, sandbox)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	t.Logf("granted in %s: %s via bridge pod %s", time.Since(started).Round(time.Millisecond), endpoint.Address, client.target.Name)
	identity, err := os.ReadFile(endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatal(err)
	}
	// The same key, re-encoded: Alpine's OpenSSH cannot load the PKCS#8 PEM
	// the bridges write (Hermes's OpenSSH can), so hand it the OpenSSH form.
	raw, err := ssh.ParseRawPrivateKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	identity = pem.EncodeToMemory(block)
	for _, pod := range []string{harness, outsider} {
		kubectlOK(t, bytes.NewReader(identity), "exec", "-i", "-n", testNamespace, pod, "--", "sh", "-c", "umask 077 && cat > /tmp/id")
	}
	out, err := sshFrom(harness, endpoint.Address, "bash -c 'hostname; echo tool-call-ok'")
	if err != nil || !strings.Contains(out, sandboxPod) || !strings.Contains(out, "tool-call-ok") {
		t.Fatalf("tool call from the harness pod: %q %v", out, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, sandboxPod) {
			t.Logf("tool call ran in %s", strings.TrimSpace(line))
		}
	}

	// 2. The same key from a pod that is not a harness is stopped by the
	// NetworkPolicy before SSH authentication is even reached.
	if out, err := sshFrom(outsider, endpoint.Address, "true"); err == nil {
		t.Errorf("a non-harness pod reached the bridge: %q", out)
	} else if !strings.Contains(out, "timed out") {
		// It holds the right key, so anything but a connect timeout would mean
		// the policy let it through.
		t.Errorf("non-harness pod failed for the wrong reason: %q", out)
	} else {
		t.Logf("non-harness pod refused: %s", firstLine(out))
	}

	// 3. Stop revokes, removes the runner's key and brings the log home.
	started = time.Now()
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	t.Logf("revoked and collected in %s", time.Since(started).Round(time.Millisecond))
	log, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil || !strings.Contains(string(log), "tool-call-ok") && !strings.Contains(string(log), `"request_type":"exec"`) {
		t.Fatalf("collected log: %q %v", log, err)
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("identity survived revocation: %v", err)
	}
	if out, err := sshFrom(harness, endpoint.Address, "true"); err == nil {
		t.Errorf("the bridge still served after revocation: %q", out)
	}

	// 4. Deleting the bridge pod mid-grant: revocation is proven by absence,
	// the logs are reported lost, and a replacement pod comes up.
	// A separate output root, as a separate run would have: the first grant's
	// known_hosts is kept as evidence and written exclusively.
	second := options
	second.OutputDir = filepath.Join(output, "runs-2")
	lost, err := New(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lost.Start(ctx, sandbox); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	kubectlOK(t, nil, "delete", "pod", "-n", testNamespace, lost.target.Name, "--wait=true")
	err = lost.Stop(ctx)
	if err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("stop after the bridge pod was deleted = %v, want revocation proven with lost evidence", err)
	}
	t.Logf("bridge pod deleted mid-grant: %v", err)
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := Preflight(ctx, options); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no replacement bridge pod became ready")
		}
		time.Sleep(3 * time.Second)
	}
}

// startClientPod runs an Alpine pod with an SSH client. harness decides
// whether it carries the labels the bridge's NetworkPolicy admits.
func startClientPod(t *testing.T, name string, harness bool) string {
	t.Helper()
	component := "it-outsider"
	if harness {
		component = "harness"
	}
	manifest := fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":%q,
"labels":{"app.kubernetes.io/managed-by":"aries","app.kubernetes.io/component":%q}},
"spec":{"restartPolicy":"Never","terminationGracePeriodSeconds":1,"automountServiceAccountToken":false,
"containers":[{"name":"client","image":"alpine:3.20","command":["sh","-c","apk add --no-cache openssh-client >/dev/null && touch /tmp/ready && sleep 3600"]}]}}`,
		name, testNamespace, component)
	kubectlOK(t, strings.NewReader(manifest), "apply", "-f", "-")
	t.Cleanup(func() { _, _ = kubectl(nil, "delete", "pod", "-n", testNamespace, name, "--wait=false") })
	kubectlOK(t, nil, "wait", "-n", testNamespace, "--for=condition=Ready", "pod/"+name, "--timeout=180s")
	// The client installs openssh-client from the Alpine mirror at start-up,
	// which has been seen to stall for minutes on a fresh node.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if _, err := kubectl(nil, "exec", "-n", testNamespace, name, "--", "test", "-f", "/tmp/ready"); err == nil {
			return name
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never installed an SSH client", name)
		}
		time.Sleep(2 * time.Second)
	}
}

func sshFrom(pod, address, command string) (string, error) {
	host, port, _ := strings.Cut(address, ":")
	return kubectl(nil, "exec", "-n", testNamespace, pod, "--", "ssh", "-i", "/tmp/id", "-p", port,
		"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=/tmp/known_hosts",
		"-o", "ConnectTimeout=8", "-o", "BatchMode=yes", "aries@"+host, command)
}

func kubectl(stdin *bytes.Reader, args ...string) (string, error) {
	command := exec.Command("kubectl", args...)
	if stdin != nil {
		command.Stdin = stdin
	}
	out, err := command.CombinedOutput()
	return string(out), err
}

func kubectlOK(t *testing.T, stdin interface{ Read([]byte) (int, error) }, args ...string) string {
	t.Helper()
	command := exec.Command("kubectl", args...)
	if stdin != nil {
		command.Stdin = stdin
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return line
}
