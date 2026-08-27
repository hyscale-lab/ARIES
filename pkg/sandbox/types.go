// Package sandbox holds the runtime-neutral value types shared by concrete
// sandbox backends (Docker, Kubernetes) and the tool bridge. Keeping them here
// lets the bridge depend on one stable type set rather than any single
// backend's package, so backends are genuinely substitutable.
package sandbox

import (
	"os"
	"time"
)

// FileInfo is one backend-neutral filesystem entry returned by a sandbox's
// stat and list operations.
type FileInfo struct {
	Name       string
	Path       string
	Type       string
	Size       int64
	Mode       os.FileMode
	ModTime    time.Time
	LinkTarget string
}

// ProcessRef identifies one attached process started in a sandbox. PID is the
// process identifier inside the sandbox. Handle carries backend-specific
// bookkeeping opaque to the bridge; each backend type-asserts it back to its
// own concrete handle when signalling or terminating the process.
type ProcessRef struct {
	PID    int
	Handle any
}
