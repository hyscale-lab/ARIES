package openclawssh

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

type supervisedTestSandbox struct {
	contractSandbox
	start   func(context.Context, string) error
	stop    func(context.Context) error
	execute func(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

func (sandbox *supervisedTestSandbox) StartAgentSession(ctx context.Context, path string) error {
	if sandbox.start != nil {
		return sandbox.start(ctx, path)
	}
	return nil
}
func (sandbox *supervisedTestSandbox) StopAgentSession(ctx context.Context) error {
	if sandbox.stop != nil {
		return sandbox.stop(ctx)
	}
	return nil
}
func (sandbox *supervisedTestSandbox) ExecAgentStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	if sandbox.execute != nil {
		return sandbox.execute(ctx, command, stdin, stdout, stderr)
	}
	return core.CommandResult{}, nil
}

// An adapter must never fall back to the benchmark's unsupervised execution.
func (*supervisedTestSandbox) ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error) {
	return core.CommandResult{ExitCode: -1}, errors.New("unsupervised tool execution")
}

func TestAgentSessionStartsBeforeSSHAccess(t *testing.T) {
	manager := newSupervisionManager(t, t.TempDir())
	started := false
	sandbox := &supervisedTestSandbox{start: func(_ context.Context, path string) error {
		if !filepath.IsAbs(path) || filepath.Base(path) != "aries-exec" {
			t.Errorf("supervisor path = %q", path)
		}
		started = true
		return nil
	}}
	manager.afterStart = func(*bridgeSession) error {
		if !started {
			t.Error("SSH access started before agent supervision")
		}
		return nil
	}
	if _, err := manager.Start(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("agent session was never started")
	}
}

func TestStopWaitsForAgentCleanupBeforeRemovingIdentity(t *testing.T) {
	manager := newSupervisionManager(t, t.TempDir())
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	sandbox := &supervisedTestSandbox{stop: func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- manager.Stop(ctx) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("Stop did not request agent cleanup")
	}
	select {
	case err := <-finished:
		t.Fatalf("Stop returned before cleanup proof: %v", err)
	default:
	}
	if _, err := os.Lstat(endpoint.IdentitySourceFile); err != nil {
		t.Fatalf("identity cleanup preceded agent cleanup confirmation: %v", err)
	}
	unblock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity remains after cleanup proof: %v", err)
	}
}

func TestStopRequiresPermanentAgentCleanupProof(t *testing.T) {
	for _, cause := range []error{errors.New("descendant cleanup proof missing"), context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			manager := newSupervisionManager(t, t.TempDir())
			stopCalls := 0
			sandbox := &supervisedTestSandbox{stop: func(context.Context) error {
				stopCalls++
				if stopCalls == 1 {
					return cause
				}
				return nil
			}}
			endpoint, err := manager.Start(context.Background(), sandbox)
			if err != nil {
				t.Fatal(err)
			}
			for attempt := range 2 {
				if err := manager.Stop(context.Background()); !errors.Is(err, cause) {
					t.Fatalf("Stop attempt %d = %v, want permanent cleanup failure %v", attempt, err, cause)
				}
			}
			if stopCalls != 2 {
				t.Fatalf("agent Stop calls = %d, want cleanup retry", stopCalls)
			}
			if _, err := os.Lstat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed proof left host identity: %v", err)
			}
		})
	}
}

func TestStartFailureStopsAttemptedAgentSession(t *testing.T) {
	for _, phase := range []string{"agent startup", "after SSH startup"} {
		t.Run(phase, func(t *testing.T) {
			outputDir := t.TempDir()
			manager := newSupervisionManager(t, outputDir)
			cause := errors.New(phase)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			startCalls, stopCalls := 0, 0
			sandbox := &supervisedTestSandbox{
				start: func(context.Context, string) error {
					startCalls++
					if phase == "agent startup" {
						cancel()
						return cause
					}
					return nil
				},
				stop: func(cleanup context.Context) error {
					stopCalls++
					if cleanup.Err() != nil {
						t.Error("partial-start cleanup reused canceled context")
					}
					return nil
				},
			}
			manager.afterStart = func(*bridgeSession) error {
				if phase == "agent startup" {
					t.Error("SSH started after failed agent startup")
				}
				cancel()
				return cause
			}
			endpoint, err := manager.Start(ctx, sandbox)
			if !errors.Is(err, cause) || endpoint.Address != "" {
				t.Fatalf("Start = (%+v, %v), want no endpoint and startup failure", endpoint, err)
			}
			if startCalls != 1 || stopCalls != 1 {
				t.Fatalf("agent lifecycle calls = (%d, %d)", startCalls, stopCalls)
			}
			if manager.active != nil {
				t.Fatal("successful partial-start cleanup retained active bridge")
			}
			artifactDir := filepath.Join(outputDir, sandbox.TaskID(), "bridge")
			if _, err := os.Lstat(artifactDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial artifacts remain: %v", err)
			}
		})
	}
}

func TestStopCancelsSSHCallsBeforeAgentCleanup(t *testing.T) {
	manager := newSupervisionManager(t, t.TempDir())
	started, exited := make(chan struct{}), make(chan struct{})
	sandbox := &supervisedTestSandbox{
		execute: func(ctx context.Context, _ core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
			close(started)
			<-ctx.Done()
			close(exited)
			return core.CommandResult{ExitCode: -1}, ctx.Err()
		},
		stop: func(context.Context) error {
			select {
			case <-exited:
			default:
				t.Error("agent cleanup started before SSH call cancellation completed")
			}
			return nil
		},
	}
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	call, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer call.Close()
	if err := call.Start(encodeCanonicalTokens([]string{remoteShell, "-c", "sleep forever"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("supervised command was not called")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("Stop = %v", err)
	}
}

func newSupervisionManager(t *testing.T, outputDir string) *Manager {
	t.Helper()
	manager := newContractManager(t, outputDir)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Stop(ctx)
	})
	return manager
}
