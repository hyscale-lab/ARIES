// Package harness owns the mechanics shared by native agent harnesses.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
	ownership "github.com/hyscale-lab/aries/pkg/harness/internal/runtime"
	"github.com/sirupsen/logrus"
)

const (
	DefaultCleanupTimeout = 30 * time.Second
	DefaultStartTimeout   = 45 * time.Second
	DefaultAgentTimeout   = 20 * time.Minute
)

// RuntimeOptions contains only inputs common to all native harness managers.
type RuntimeOptions struct {
	Deployment     deployment.Deployment
	Image          string
	OutputDir      string
	CleanupTimeout time.Duration
	StartTimeout   time.Duration
	AgentTimeout   time.Duration
	Logger         *logrus.Logger
	APIKeyLookup   func(string) ([]byte, bool)
}

// Occurrence holds common task-local ownership. Managers keep native protocol
// state separately. Run uses a snapshot, never the cleanup owner's mutable data.
type Occurrence struct {
	RunID             string
	TaskID            string
	AttemptID         string
	Name              string
	ID                string
	DeploymentRequest deployment.Request
	ArtifactDir       string
	Endpoint          core.ToolEndpoint
	Model             core.ModelConfig
	AgentTimeout      time.Duration
	Credentials       *Credentials
	Artifacts         *Artifacts
}

// Runtime is the sole common admission and ownership coordinator. The native
// manager serializes publication/snapshotting of its protocol state. Its mutex
// may be acquired before Runtime's mutex, never the reverse. Runtime does not
// call native code or hold its mutex during deployment operations.
//
// Lifecycle: Own -> Start -> native readiness -> Ready -> AdmitRun.
// Any failed startup uses Rollback. Stop uses BeginStop -> native cancellation
// -> Remove -> native client closure -> FinishStop. No callback hooks are needed.
type Runtime struct {
	Options  RuntimeOptions
	label    string
	mu       sync.Mutex
	active   *Occurrence
	ready    bool
	admitted bool
	stop     ownership.StopState
	close    ownership.TransportClose
}

func NewRuntime(label string, options RuntimeOptions) (*Runtime, error) {
	if err := containerimage.ValidatePinnedTagOnly(options.Image); err != nil {
		return nil, fmt.Errorf("%s image: %w", label, err)
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, fmt.Errorf("%s output directory is required", label)
	}
	output, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve %s output directory: %w", label, err)
	}
	if err = privatefiles.EnsureDirectory(output); err != nil {
		return nil, fmt.Errorf("prepare %s output directory: %w", label, err)
	}
	if options.Deployment == nil {
		return nil, fmt.Errorf("%s deployment is required", label)
	}
	options.OutputDir = output
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = DefaultCleanupTimeout
	}
	if options.StartTimeout <= 0 {
		options.StartTimeout = DefaultStartTimeout
	}
	if options.AgentTimeout <= 0 {
		options.AgentTimeout = DefaultAgentTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	if options.APIKeyLookup == nil {
		options.APIKeyLookup = EnvironmentAPIKeyLookup
	}
	return &Runtime{Options: options, label: label}, nil
}

func (r *Runtime) InUse() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active != nil || r.stop.Running()
}
func (r *Runtime) Own(o *Occurrence) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active != nil || r.stop.Running() {
		return fmt.Errorf("%s harness is already active", r.label)
	}
	r.active = o
	r.ready = false
	r.admitted = false
	r.stop.Reset(nil)
	return nil
}
func (r *Runtime) Ready() { r.mu.Lock(); defer r.mu.Unlock(); r.ready = true }
func (r *Runtime) AdmitRun(instruction string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop.Running() {
		return fmt.Errorf("%s harness is stopping", r.label)
	}
	if r.active == nil || !r.ready {
		return fmt.Errorf("%s harness is not started", r.label)
	}
	if r.admitted {
		return fmt.Errorf("%s harness accepts exactly one task instruction", r.label)
	}
	if strings.TrimSpace(instruction) == "" || strings.ContainsRune(instruction, 0) {
		return fmt.Errorf("%s task instruction is invalid", r.label)
	}
	r.admitted = true
	return nil
}

// Start retains a returned identity even when Create reports partial failure.
// Own must precede this operation, so every failure is available to rollback.
func (r *Runtime) Start(ctx context.Context, o *Occurrence, request deployment.Request, archive []byte, secrets [][]byte) error {
	o.DeploymentRequest = request
	id, err := r.Options.Deployment.Create(ctx, request)
	o.ID = id
	if err != nil {
		return fmt.Errorf("create %s runtime: %w", r.label, err)
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("deployment returned an empty %s runtime ID", r.label)
	}
	if err = r.Options.Deployment.UploadArchive(ctx, id, "/", bytes.NewReader(archive)); err != nil {
		return fmt.Errorf("copy private %s runtime: %w", r.label, err)
	}
	if err = r.Options.Deployment.Validate(ctx, id, request, secrets); err != nil {
		return err
	}
	if err = r.Options.Deployment.Start(ctx, id); err != nil {
		return fmt.Errorf("start %s container: %w", r.label, err)
	}
	return nil
}

func (r *Runtime) BeginStop() (*ownership.StopAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Failed removal retains ownership for cleanup retries, never new runs.
	r.ready = false
	return r.stop.Begin(r.active != nil)
}
func (r *Runtime) Remove(ctx context.Context, o *Occurrence) error {
	if o == nil {
		return nil
	}
	if err := ownership.StopOwned(ctx, r.Options.Deployment, &o.ID); err != nil {
		return err
	}
	if o.Credentials != nil {
		o.Credentials.Clear()
	}
	return nil
}
func (r *Runtime) FinishStop(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil || r.active.ID == "" {
		r.active = nil
		r.ready = false
	}
	r.stop.Finish(err)
}
func (r *Runtime) Rollback(ctx context.Context, o *Occurrence, primary error) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.Options.CleanupTimeout)
	err := r.Remove(cleanup, o)
	cancel()
	r.mu.Lock()
	if err == nil {
		r.active = nil
		r.ready = false
	}
	r.stop.Reset(err)
	r.mu.Unlock()
	if err != nil {
		return errors.Join(primary, fmt.Errorf("rollback partial %s harness: %w", r.label, err))
	}
	_ = os.RemoveAll(o.ArtifactDir)
	return primary
}
func (r *Runtime) Close() error { return r.close.Close(r.Options.Deployment) }

// Snapshot preserves run evidence and secrets independently of cleanup.
func (o *Occurrence) Snapshot() *Occurrence {
	copy := *o
	if o.Credentials != nil {
		copy.Credentials = o.Credentials.Snapshot()
	}
	if o.Artifacts != nil {
		artifacts := o.Artifacts.Snapshot()
		copy.Artifacts = &artifacts
	}
	return &copy
}

// FailedResult records cancellation separately while keeping errors private.
func (o *Occurrence) FailedResult(started time.Time, err error) core.HarnessResult {
	status := core.StatusFailed
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = core.StatusCanceled
	}
	errorText := ""
	if err != nil {
		errorText = string(o.Credentials.Redact([]byte(err.Error())))
	}
	return core.HarnessResult{Status: status, Duration: time.Since(started), LogPaths: o.Artifacts.Paths(), Error: errorText}
}
