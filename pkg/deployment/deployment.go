// Package deployment defines runtime operations shared by harnesses and tool sandboxes.
package deployment

import (
	"context"
	"errors"
	"io"
	"io/fs"

	"github.com/hyscale-lab/aries/pkg/core"
)

// ErrAllocationUnconfirmed means Create could not establish allocation absence
// or return an owned runtime identity. Callers must not report successful cleanup
// when Create returns this error without an identity.
var ErrAllocationUnconfirmed = errors.New("deployment allocation unconfirmed")

// Deployment is infrastructure shared by harnesses and tool sandboxes, not a Runner component.
// Create preserves known cleanup identities and reports unresolved allocations.
// Stop succeeds only after positively confirming absence. Archive operations
// carry tar modes and ownership to the backend; harness readiness must verify
// effective runtime permissions. Downloads return runner.ErrNotFound only for a missing
// source in an existing runtime; missing log runtimes also wrap ErrNotFound.
// Exec and ExecStream preserve argv and confirm targeted process termination on cancellation.
// Address returns a host-reachable private service address, without a scheme.
type Deployment interface {
	Runtime
	Exec(context.Context, string, core.Command) (core.CommandResult, error)
	ExecStream(context.Context, string, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

// Runtime manages lifecycle, private transfers and runner-facing control addresses.
// It deliberately does not require sandbox execution capabilities.
type Runtime interface {
	// Create returns an owned identity even on partial failure when available.
	// An empty identity with ErrAllocationUnconfirmed leaves allocation absence
	// unresolved; other empty-identity failures confirm no allocation to clean up.
	Create(context.Context, Request) (string, error)
	// Validate confirms identity, ownership, isolation and absence of supplied
	// secret values from runtime metadata before exposing the runtime. It must not retain secrets.
	Validate(context.Context, string, Request, [][]byte) error
	UploadArchive(context.Context, string, string, io.Reader) error
	DownloadArchive(context.Context, string, string) (io.ReadCloser, FileInfo, error)
	Start(context.Context, string) error
	Running(context.Context, string) (bool, error)
	LogsStream(context.Context, string, io.Writer, io.Writer) error
	Logs(context.Context, string, int) ([]byte, error)
	Address(context.Context, string, int) (string, error)
	Stop(context.Context, string) error
	Close() error
}

// Request describes one private runtime. Secrets are passed
// separately to validation and must never enter environment, argv, or labels.
// Entrypoint nil preserves the image's entrypoint; an explicit value replaces it.
type Request struct {
	// InternalPort is exposed only on the task attachment, without host publication.
	InternalPort int
	// Mounts grants access only to the host paths explicitly supplied by wiring.
	// The default grants no host filesystem access.
	Mounts          []Mount
	Workdir         string
	StorageMB       int
	GPUs            int
	Init            bool
	NoNewPrivileges bool
	NetworkAliases  []string
	// AllowImageVolumes permits anonymous volumes declared by the image.
	AllowImageVolumes bool
	Name              string
	Image             string
	Env               []string
	Entrypoint        []string
	Args              []string
	Labels            map[string]string
	Placement         core.RuntimePlacement
	// Nil CPU and memory dimensions impose no runtime limit.
	CPU      *float64
	MemoryMB *int
	// ImageVolumes allows only image-declared private volumes at these paths;
	// it never grants permission to mount host files.
	ImageVolumes []string
	// ServicePort publishes a private TCP service; zero publishes none.
	ServicePort int
}

// Mount declares one host path made available at a runtime path.
// The provider preserves Source, Target, and read-only semantics exactly.
type Mount struct {
	Source, Target string
	ReadOnly       bool
}

type FileInfo struct {
	Size int64
	Mode fs.FileMode
}

// RunEnvironment owns connectivity shared by all runtimes in a run. It is
// stopped after the last runtime, independently of logical task handles.
type RunEnvironment interface {
	Start(context.Context) (core.RuntimePlacement, error)
	NewTaskEnvironment() TaskEnvironment
	NetworkID() string
	NetworkName() string
	Stop(context.Context) error
}

// TaskEnvironmentRequest supplies the unique runtime service identity.
type TaskEnvironmentRequest struct {
	core.SandboxRequest
	RuntimeName string
}

// TaskEnvironment is a fresh logical attachment for one sandbox. Stop releases
// the handle without removing infrastructure owned by the run.
type TaskEnvironment interface {
	Start(context.Context, TaskEnvironmentRequest) (core.HarnessConnectivity, error)
	Validate(context.Context) error
	Stop(context.Context) error
}

// ServiceRuntime resolves a task-attachment address independently of host control.
type ServiceRuntime interface {
	TaskAddress(context.Context, string, int) (string, error)
}
