// Package lifecycle composes a harness protocol adapter with the tool sandbox
// it exposes, so one ToolBridge owns the task sandbox from creation through
// removal and decides whether it stays running between tool calls.
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// Mode selects how the bridge keeps the task sandbox between tool calls.
type Mode string

const (
	// Persistent keeps the sandbox running for the whole task.
	Persistent Mode = "persistent"
	// Checkpoint suspends the sandbox while access is granted and no tool
	// call is active, restoring it before each call.
	Checkpoint Mode = "checkpoint"
)

const (
	defaultCleanupTimeout = 30 * time.Second
	// The events log stays outside the access adapter's artifact directory,
	// which a failed grant removes.
	eventsDirectory = "checkpoint"
	eventsFileName  = "events.jsonl"
)

// ParseMode accepts a configured mode; empty selects Persistent.
func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case "", Persistent:
		return Persistent, nil
	case Checkpoint:
		return Checkpoint, nil
	default:
		return "", fmt.Errorf("unsupported sandbox lifecycle %q", value)
	}
}

// Access grants one harness protocol access to a live sandbox. A nil Stop
// error confirms that access is revoked and its commands are drained.
type Access interface {
	Start(context.Context, runner.Sandbox) (core.ToolEndpoint, error)
	Stop(context.Context) error
}

// Options configures a bridge. New takes no ownership of ToolSandbox or Access
// beyond the per-task lifecycle driven through the bridge.
type Options struct {
	ToolSandbox    runner.ToolSandbox
	Access         Access
	Mode           Mode
	OutputDir      string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// Bridge implements runner.ToolBridge for one task occurrence at a time.
type Bridge struct {
	toolSandbox    runner.ToolSandbox
	access         Access
	mode           Mode
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger

	mu         sync.Mutex
	live       runner.Sandbox
	exposed    runner.Sandbox
	suspending *suspendingSandbox
}

var _ runner.ToolBridge = (*Bridge)(nil)

// New validates options without creating any task resource.
func New(options Options) (*Bridge, error) {
	if options.ToolSandbox == nil {
		return nil, errors.New("bridge tool sandbox is required")
	}
	if options.Access == nil {
		return nil, errors.New("bridge access adapter is required")
	}
	mode, err := ParseMode(string(options.Mode))
	if err != nil {
		return nil, err
	}
	if mode == Checkpoint && strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("bridge output directory is required for checkpoint evidence")
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Bridge{
		toolSandbox: options.ToolSandbox, access: options.Access, mode: mode,
		outputDir: options.OutputDir, cleanupTimeout: options.CleanupTimeout, logger: options.Logger,
	}, nil
}

// Open creates the task sandbox. Close must follow every attempt.
func (b *Bridge) Open(ctx context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live != nil {
		return nil, errors.New("bridge sandbox is already open")
	}
	if b.mode == Checkpoint {
		if err := validateCheckpointEnvironment(request.Environment); err != nil {
			return nil, err
		}
	}
	live, err := b.toolSandbox.Start(ctx, request)
	if err != nil {
		return nil, err
	}
	if live == nil {
		return nil, errors.New("tool sandbox returned a nil sandbox")
	}
	b.live, b.exposed = live, live
	if b.mode == Persistent {
		return live, nil
	}
	suspending, err := newSuspendingSandbox(live, filepath.Join(b.outputDir, request.TaskID, eventsDirectory, eventsFileName), b.cleanupTimeout)
	if err != nil {
		return nil, err
	}
	b.suspending, b.exposed = suspending, suspending
	b.logger.WithContext(ctx).WithField("task_id", request.TaskID).Info("bridge sandbox checkpoints between tool calls")
	return suspending, nil
}

// Start grants harness access. In checkpoint mode the idle sandbox is
// suspended first, so a checkpoint failure fails the grant.
func (b *Bridge) Start(ctx context.Context) (core.ToolEndpoint, error) {
	b.mu.Lock()
	exposed, suspending := b.exposed, b.suspending
	b.mu.Unlock()
	if exposed == nil {
		return core.ToolEndpoint{}, errors.New("bridge sandbox is not open")
	}
	if suspending != nil {
		if err := suspending.grant(ctx); err != nil {
			return core.ToolEndpoint{}, err
		}
	}
	endpoint, err := b.access.Start(ctx, exposed)
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	if suspending != nil {
		endpoint.LogPaths = append(endpoint.LogPaths, suspending.eventsPath)
	}
	return endpoint, nil
}

// Stop revokes access, then ends checkpointing and waits for an in-flight
// checkpoint. A checkpoint or evidence failure during the grant is returned
// so the run does not evaluate a sandbox whose suspension was unconfirmed.
// The sandbox stays available for evaluation, restored on first use.
func (b *Bridge) Stop(ctx context.Context) error {
	if err := b.access.Stop(ctx); err != nil {
		return err
	}
	b.mu.Lock()
	suspending := b.suspending
	b.mu.Unlock()
	if suspending == nil {
		return nil
	}
	return suspending.revoke(ctx)
}

// Close removes the sandbox; a nil error confirms absence. Repeated calls are safe.
func (b *Bridge) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live == nil {
		return nil
	}
	var evidenceErr error
	if b.suspending != nil {
		// A failed revocation must not leave a checkpoint racing removal.
		if err := b.suspending.revoke(ctx); ctx.Err() != nil {
			return err
		}
		evidenceErr = b.suspending.closeEvents()
	}
	if err := b.toolSandbox.Stop(ctx, b.live); err != nil {
		return errors.Join(err, evidenceErr)
	}
	b.live, b.exposed, b.suspending = nil, nil, nil
	return evidenceErr
}

func validateCheckpointEnvironment(environment core.Environment) error {
	if environment.Services.SearchPort != 0 {
		return errors.New("checkpoint sandbox lifecycle cannot suspend a task that serves the harness directly")
	}
	if environment.GPUs != 0 {
		return errors.New("checkpoint sandbox lifecycle does not support GPU tasks")
	}
	return nil
}

// checkpointableSandbox is the live capability the checkpoint mode wraps. It
// carries the streaming, bounded-download, and identity capabilities that the
// paired SSH adapters and benchmarks may require.
type checkpointableSandbox interface {
	runner.Sandbox
	runner.StreamExecutor
	runner.LimitedDownloader
	runner.Checkpointer
	ContainerID() string
	ContainerName() string
	RunID() string
	TaskID() string
	Workdir() string
}

// suspendingSandbox restores the sandbox before each operation and, while
// access is granted, checkpoints it once no operation remains active. The
// checkpoint runs after the operation returns, so tool-call latency includes
// restore but not checkpoint time.
type suspendingSandbox struct {
	live           checkpointableSandbox
	eventsPath     string
	cleanupTimeout time.Duration

	// mu is held across checkpoint and restore so an operation never starts
	// against a sandbox that is being suspended.
	mu        sync.Mutex
	active    int
	granted   bool
	suspended bool
	failure   error
	pending   sync.WaitGroup
	events    *os.File
	sequence  uint64
}

type checkpointEvent struct {
	Sequence   uint64 `json:"sequence"`
	Event      string `json:"event"`
	Timestamp  string `json:"timestamp"`
	DurationNS int64  `json:"duration_ns"`
	Granted    bool   `json:"granted"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	RunID      string `json:"run_id"`
	TaskID     string `json:"task_id"`
	Container  string `json:"container_id"`
}

var (
	_ runner.Sandbox           = (*suspendingSandbox)(nil)
	_ runner.StreamExecutor    = (*suspendingSandbox)(nil)
	_ runner.LimitedDownloader = (*suspendingSandbox)(nil)
)

func newSuspendingSandbox(live runner.Sandbox, eventsPath string, cleanupTimeout time.Duration) (*suspendingSandbox, error) {
	checkpointable, ok := live.(checkpointableSandbox)
	if !ok {
		return nil, fmt.Errorf("checkpoint sandbox lifecycle requires a checkpointing streaming sandbox, got %T", live)
	}
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint evidence directory: %w", err)
	}
	events, err := os.OpenFile(eventsPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create checkpoint evidence: %w", err)
	}
	return &suspendingSandbox{live: checkpointable, eventsPath: eventsPath, cleanupTimeout: cleanupTimeout, events: events}, nil
}

func (s *suspendingSandbox) grant(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.granted = true
	if s.active != 0 || s.suspended {
		return nil
	}
	return s.checkpointLocked(ctx)
}

func (s *suspendingSandbox) revoke(ctx context.Context) error {
	s.mu.Lock()
	s.granted = false
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.pending.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return fmt.Errorf("await sandbox checkpoint: %w", ctx.Err())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *suspendingSandbox) acquire(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspended {
		started := time.Now()
		err := s.live.Restore(ctx)
		s.recordLocked("restore", started, err)
		if err != nil {
			// Restore leaves a running sandbox alone, so the next acquire retries safely.
			return fmt.Errorf("restore suspended sandbox: %w", err)
		}
		s.suspended = false
	}
	s.active++
	return nil
}

func (s *suspendingSandbox) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active--
	if s.active != 0 || !s.granted {
		return
	}
	// The checkpoint is committed while access is granted; revocation waits
	// for it instead of cancelling it, so every granted tool call is followed
	// by a recorded checkpoint unless another call begins first.
	s.pending.Add(1)
	go func() {
		defer s.pending.Done()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.active != 0 || s.suspended {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.cleanupTimeout)
		defer cancel()
		_ = s.checkpointLocked(ctx)
	}()
}

func (s *suspendingSandbox) checkpointLocked(ctx context.Context) error {
	started := time.Now()
	err := s.live.Checkpoint(ctx)
	s.recordLocked("checkpoint", started, err)
	// A failed checkpoint may still have stopped the sandbox; the next acquire
	// restores it, which refuses to resume from an older checkpoint.
	s.suspended = true
	if err != nil {
		err = fmt.Errorf("checkpoint idle sandbox: %w", err)
		s.failure = errors.Join(s.failure, err)
	}
	return err
}

func (s *suspendingSandbox) recordLocked(event string, started time.Time, operationErr error) {
	s.sequence++
	record := checkpointEvent{
		Sequence: s.sequence, Event: event, Timestamp: started.UTC().Format(time.RFC3339Nano),
		DurationNS: time.Since(started).Nanoseconds(), Granted: s.granted, Status: core.StatusSucceeded,
		RunID: s.live.RunID(), TaskID: s.live.TaskID(), Container: s.live.ContainerID(),
	}
	if operationErr != nil {
		record.Status, record.Error = core.StatusFailed, operationErr.Error()
	}
	if s.events == nil {
		s.failure = errors.Join(s.failure, errors.New("record checkpoint event: evidence is closed"))
		return
	}
	line, err := json.Marshal(record)
	if err == nil {
		_, err = s.events.Write(append(line, '\n'))
	}
	if err != nil {
		s.failure = errors.Join(s.failure, fmt.Errorf("record checkpoint event: %w", err))
	}
}

func (s *suspendingSandbox) closeEvents() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		return nil
	}
	err := errors.Join(s.events.Sync(), s.events.Close())
	s.events = nil
	if err != nil {
		return fmt.Errorf("close checkpoint evidence: %w", err)
	}
	return nil
}

func (s *suspendingSandbox) Connectivity() core.HarnessConnectivity { return s.live.Connectivity() }
func (s *suspendingSandbox) ContainerID() string                    { return s.live.ContainerID() }
func (s *suspendingSandbox) ContainerName() string                  { return s.live.ContainerName() }
func (s *suspendingSandbox) RunID() string                          { return s.live.RunID() }
func (s *suspendingSandbox) TaskID() string                         { return s.live.TaskID() }
func (s *suspendingSandbox) Workdir() string                        { return s.live.Workdir() }

func (s *suspendingSandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	if err := s.acquire(ctx); err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	defer s.release()
	return s.live.Exec(ctx, command)
}

func (s *suspendingSandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	if err := s.acquire(ctx); err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	defer s.release()
	return s.live.ExecStream(ctx, command, stdin, stdout, stderr)
}

func (s *suspendingSandbox) Upload(ctx context.Context, source, destination string) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	return s.live.Upload(ctx, source, destination)
}

func (s *suspendingSandbox) Download(ctx context.Context, source, destination string) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	return s.live.Download(ctx, source, destination)
}

func (s *suspendingSandbox) DownloadLimit(ctx context.Context, source, destination string, maxBytes int64) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	return s.live.DownloadLimit(ctx, source, destination, maxBytes)
}
