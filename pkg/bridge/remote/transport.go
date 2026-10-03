package remote

import (
	"context"
	"io"

	"github.com/hyscale-lab/aries/pkg/runner"
)

// Transport is how the runner reaches the bridge, and the only part of the
// client that differs between deployments. The grant lifecycle, revocation
// and evidence handling above it are the same everywhere.
//
//	KubeTransport     the aries-bridge pod, reached with kubectl exec
//	DockerTransport   the aries-bridge container, reached with docker exec
type Transport interface {
	// Describe names the sandbox in the terms the bridge attaches by, and
	// refuses a sandbox this deployment cannot serve.
	Describe(runner.Sandbox) (SandboxRef, error)
	// Locate finds the one bridge to grant on.
	Locate(context.Context) (Target, error)
	// Exec runs `aries-bridge <args>` in the bridge, with stdin and stdout
	// streamed. A non-zero exit is an error carrying stderr.
	Exec(ctx context.Context, target Target, stdin io.Reader, stdout io.Writer, args ...string) error
	// Gone reports whether the process that held target's grants provably no
	// longer exists: its container or pod is gone, stopped or replaced. It is
	// the only other way, besides the daemon's own answer, to prove a grant
	// revoked, since grants live only in the daemon's memory.
	Gone(context.Context, Target) (bool, error)
	// Join makes the bridge reachable from the sandbox's harness and returns
	// the address the grant must listen and be advertised on. Empty lets the
	// bridge use its own.
	Join(context.Context, Target, SandboxRef) (string, error)
	// Leave undoes Join. It succeeds when there is nothing to undo.
	Leave(context.Context, Target, SandboxRef) error
}

// Target is one located bridge. Identity distinguishes a replacement from
// the original even when the name is reused.
type Target struct {
	Name     string
	Identity string
}

// SandboxRef names a task sandbox. Backend selects which fields apply:
// "kubernetes" uses Namespace, PodName and SandboxID; "docker" uses
// ContainerID and Network.
type SandboxRef struct {
	Backend     string `json:"backend"`
	Namespace   string `json:"namespace,omitempty"`
	PodName     string `json:"pod_name,omitempty"`
	SandboxID   string `json:"sandbox_id,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
	Network     string `json:"network,omitempty"`
	Workdir     string `json:"workdir"`
	RunID       string `json:"run_id"`
	TaskID      string `json:"task_id"`
}

// Sandbox backends a SandboxRef can name.
const (
	BackendKubernetes = "kubernetes"
	BackendDocker     = "docker"
)
