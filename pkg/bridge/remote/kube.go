package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/hyscale-lab/aries/pkg/runner"
)

const defaultKubectl = "kubectl"

// Kubectl runs one kubectl command. stdin and stdout may be nil.
type Kubectl interface {
	Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
}

// KubeTransport reaches the aries-bridge pod through the API server with
// kubectl exec, so nothing is exposed on the pod network and RBAC decides who
// may drive the bridge.
type KubeTransport struct {
	// Namespace is where the bridge pod runs. Grants may only name sandboxes
	// in the same namespace.
	Namespace string
	Kubectl   Kubectl
}

// NewKubeTransport uses the kubectl binary at path, or "kubectl".
func NewKubeTransport(namespace, path string) *KubeTransport {
	return &KubeTransport{Namespace: namespace, Kubectl: ExecKubectl{Path: path}}
}

// kubeSandbox is what the runner must know about a sandbox to have the
// bridge pod attach to it. The Kubernetes sandbox satisfies it.
type kubeSandbox interface {
	runner.Sandbox
	Namespace() string
	ContainerName() string
	SandboxID() string
	Workdir() string
	RunID() string
	TaskID() string
}

// Describe names a Kubernetes sandbox pod.
func (t *KubeTransport) Describe(generic runner.Sandbox) (SandboxRef, error) {
	sandbox, ok := generic.(kubeSandbox)
	if !ok {
		return SandboxRef{}, errors.New("the bridge pod can only serve a Kubernetes sandbox")
	}
	if sandbox.Namespace() != t.Namespace {
		return SandboxRef{}, fmt.Errorf("sandbox runs in namespace %q but the bridge pod serves %q", sandbox.Namespace(), t.Namespace)
	}
	return SandboxRef{
		Backend: BackendKubernetes, Namespace: sandbox.Namespace(), PodName: sandbox.ContainerName(),
		SandboxID: sandbox.SandboxID(), Workdir: sandbox.Workdir(), RunID: sandbox.RunID(), TaskID: sandbox.TaskID(),
	}, nil
}

type podList struct {
	Items []podInfo `json:"items"`
}

type podInfo struct {
	Metadata struct {
		Name              string  `json:"name"`
		UID               string  `json:"uid"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// Locate returns the one ready bridge pod. It is looked up per grant, so a
// bridge pod replaced between tasks is simply picked up by the next one.
func (t *KubeTransport) Locate(ctx context.Context) (Target, error) {
	var stdout bytes.Buffer
	if err := t.Kubectl.Run(ctx, nil, &stdout, "get", "pods", "-n", t.Namespace, "-l", PodSelector, "-o", "json"); err != nil {
		return Target{}, fmt.Errorf("find the bridge pod: %w", err)
	}
	var list podList
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		return Target{}, fmt.Errorf("parse bridge pods: %w", err)
	}
	var ready []Target
	for _, pod := range list.Items {
		if pod.Metadata.DeletionTimestamp != nil || pod.Status.Phase != "Running" {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == ContainerName && status.Ready {
				ready = append(ready, Target{Name: pod.Metadata.Name, Identity: pod.Metadata.UID})
			}
		}
	}
	if len(ready) != 1 {
		return Target{}, fmt.Errorf("need exactly one ready bridge pod (%s) in namespace %s, found %d; is the ARIES chart deployed?", PodSelector, t.Namespace, len(ready))
	}
	return ready[0], nil
}

// Exec runs aries-bridge in the bridge pod's container.
func (t *KubeTransport) Exec(ctx context.Context, target Target, stdin io.Reader, stdout io.Writer, args ...string) error {
	kubectlArgs := []string{"exec"}
	if stdin != nil {
		kubectlArgs = append(kubectlArgs, "-i")
	}
	kubectlArgs = append(kubectlArgs, "-n", t.Namespace, target.Name, "-c", ContainerName, "--", Executable)
	return t.Kubectl.Run(ctx, stdin, stdout, append(kubectlArgs, args...)...)
}

// Gone reports whether the pod no longer exists or has a different UID.
func (t *KubeTransport) Gone(ctx context.Context, target Target) (bool, error) {
	var stdout bytes.Buffer
	if err := t.Kubectl.Run(ctx, nil, &stdout, "get", "pod", "-n", t.Namespace, target.Name, "--ignore-not-found", "-o", "jsonpath={.metadata.uid}"); err != nil {
		return false, fmt.Errorf("check bridge pod %s: %w", target.Name, err)
	}
	return strings.TrimSpace(stdout.String()) != target.Identity, nil
}

// Join is a no-op: harness pods reach the bridge pod on the flat pod network,
// gated by its NetworkPolicy, and the pod advertises its own IP.
func (*KubeTransport) Join(context.Context, Target, SandboxRef) (string, error) { return "", nil }

// Leave is a no-op, as Join is.
func (*KubeTransport) Leave(context.Context, Target, SandboxRef) error { return nil }

// ExecKubectl runs the kubectl binary. Its kubeconfig is the runner's
// cluster contract, as for the sandbox and harness backends.
type ExecKubectl struct{ Path string }

// Run implements Kubectl, reporting stderr in the error.
func (k ExecKubectl) Run(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	path := k.Path
	if path == "" {
		path = defaultKubectl
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin = stdin
	if stdout == nil {
		stdout = io.Discard
	}
	command.Stdout = stdout
	var stderr bytes.Buffer
	command.Stderr = &truncatingWriter{writer: &stderr, remaining: 64 << 10}
	if err := command.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return fmt.Errorf("kubectl %s: %w: %s", args[0], err, message)
		}
		return fmt.Errorf("kubectl %s: %w", args[0], err)
	}
	return nil
}
