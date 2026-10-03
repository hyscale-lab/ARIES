// Package deployment defines runtime operations shared by harnesses and tool sandboxes.
package deployment

import (
	"context"
	"io"
	"io/fs"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Deployment is infrastructure shared by harnesses and tool sandboxes, not a Runner component.
// Create returns an identity even on partial failure when cleanup is required.
// Stop succeeds only after positively confirming absence. Archive operations
// carry tar modes and ownership to the backend; harness readiness must verify
// effective runtime permissions. Downloads return runner.ErrNotFound only for a missing
// source in an existing runtime; missing log runtimes also wrap ErrNotFound.
// Exec and ExecStream preserve argv and confirm targeted process termination on cancellation.
// Address returns a host-reachable private service address, without a scheme.
type Deployment interface {
	Create(context.Context, Request) (string, error)
	// Validate confirms identity, ownership, isolation and absence of supplied
	// secret values from runtime metadata before exposing the runtime. It must not retain secrets.
	Validate(context.Context, string, Request, [][]byte) error
	UploadArchive(context.Context, string, string, io.Reader) error
	DownloadArchive(context.Context, string, string) (io.ReadCloser, FileInfo, error)
	Start(context.Context, string) error
	Running(context.Context, string) (bool, error)
	Exec(context.Context, string, core.Command) (core.CommandResult, error)
	ExecStream(context.Context, string, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
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
	Network           string
	// Nil CPU and memory dimensions impose no runtime limit.
	CPU      *float64
	MemoryMB *int
	// ImageVolumes allows only image-declared private volumes at these paths;
	// it never grants permission to mount host files.
	ImageVolumes []string
	// ServicePort publishes a private TCP service; zero publishes none.
	ServicePort int
}

type FileInfo struct {
	Size int64
	Mode fs.FileMode
}

// TaskEnvironment owns the task attachment beneath the four Runner roles.
// Start may fail after allocation; callers must still call Stop. Stop succeeds
// only after confirming absence. Each task occurrence requires a fresh owner.
type TaskEnvironment interface {
	Start(context.Context, core.SandboxRequest) (string, error)
	Validate(context.Context) error
	Stop(context.Context) error
	BridgeListen(context.Context) (core.BridgeListen, error)
}
