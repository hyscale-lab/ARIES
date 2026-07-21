package openclawssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

type fakeBridgeSandbox struct {
	runtimeDir           string
	workdir              string
	commands             []core.Command
	mu                   sync.Mutex
	process              atomic.Bool
	restarts             atomic.Int32
	prepareErr           error
	prepareExit          int
	recoveryErr          error
	recoverContextActive atomic.Bool
	processErr           error
}

func newFakeBridgeSandbox(t *testing.T) *fakeBridgeSandbox {
	t.Helper()
	sandbox := &fakeBridgeSandbox{runtimeDir: t.TempDir(), workdir: "/work"}
	return sandbox
}

func (sandbox *fakeBridgeSandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	command.Args = append([]string(nil), command.Args...)
	command.Stdin = append([]byte(nil), command.Stdin...)
	sandbox.mu.Lock()
	sandbox.commands = append(sandbox.commands, command)
	sandbox.mu.Unlock()
	if command.Path == serverContainerPath && len(command.Args) > 0 && command.Args[0] == "spawn" {
		sandbox.process.Store(true)
	}
	if command.Path == serverContainerPath && len(command.Args) > 0 && command.Args[0] == "prepare" && sandbox.prepareErr != nil {
		return core.CommandResult{ExitCode: -1}, sandbox.prepareErr
	}
	if command.Path == serverContainerPath && len(command.Args) > 0 && command.Args[0] == "prepare" && sandbox.prepareExit != 0 {
		return core.CommandResult{ExitCode: sandbox.prepareExit}, nil
	}
	if command.Path == trustedExecHelper && len(command.Args) > 0 && command.Args[0] == "--recover-workspace" && ctx.Err() == nil {
		sandbox.recoverContextActive.Store(true)
		if sandbox.recoveryErr != nil {
			return core.CommandResult{ExitCode: 125}, sandbox.recoveryErr
		}
	}
	return core.CommandResult{ExitCode: 0}, nil
}

func (sandbox *fakeBridgeSandbox) Upload(context.Context, string, string) error   { return nil }
func (sandbox *fakeBridgeSandbox) Download(context.Context, string, string) error { return nil }
func (sandbox *fakeBridgeSandbox) Stop(context.Context) error                     { return nil }
func (sandbox *fakeBridgeSandbox) NetworkName() string                            { return "aries-net-test" }
func (sandbox *fakeBridgeSandbox) Workdir() string                                { return sandbox.workdir }
func (sandbox *fakeBridgeSandbox) RuntimeDir() string                             { return sandbox.runtimeDir }
func (sandbox *fakeBridgeSandbox) ContainerIPv4(context.Context) (string, error) {
	return "127.0.0.1", nil
}
func (sandbox *fakeBridgeSandbox) ProcessPresent(context.Context, int) (bool, error) {
	return sandbox.process.Load(), sandbox.processErr
}
func (sandbox *fakeBridgeSandbox) RestartForIsolation(context.Context) error {
	sandbox.restarts.Add(1)
	sandbox.process.Store(false)
	return nil
}

func (sandbox *fakeBridgeSandbox) commandModes() []string {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	modes := make([]string, 0, len(sandbox.commands))
	for _, command := range sandbox.commands {
		if command.Path == serverContainerPath && len(command.Args) > 0 {
			modes = append(modes, command.Args[0])
		} else if command.Path == trustedExecHelper && len(command.Args) > 0 {
			modes = append(modes, command.Args[0])
		} else {
			modes = append(modes, filepath.Base(command.Path))
		}
	}
	return modes
}

func (sandbox *fakeBridgeSandbox) commandsSnapshot() []core.Command {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	return append([]core.Command(nil), sandbox.commands...)
}

func testManager(t *testing.T, sandbox *fakeBridgeSandbox) *Manager {
	t.Helper()
	helperDir := t.TempDir()
	client := filepath.Join(helperDir, "aries-ssh")
	server := filepath.Join(helperDir, "aries-ssh-server")
	for _, path := range []string{client, server} {
		if err := os.WriteFile(path, []byte("static-helper"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := New(Options{
		OutputDir: t.TempDir(), ClientPath: client, ServerPath: server,
		CleanupTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.newID = func() (string, error) { return "fixedid", nil }
	manager.probe = func(context.Context, string, ssh.Signer, ssh.PublicKey) error { return nil }
	manager.waitListener = func(context.Context, string) error { return nil }
	manager.oldKeyRejected = func(context.Context, *bridgeSession) error { return nil }
	manager.startServer = func(ctx context.Context, generic bridgeSandbox, _, controlContainer, workspace string, token, _, _ []byte) (io.Closer, int, bool, error) {
		result, err := generic.Exec(ctx, core.Command{
			Path: serverContainerPath,
			Args: []string{"spawn", "--control", controlContainer, "--listen", net.JoinHostPort("0.0.0.0", "2222"), "--workspace", workspace},
			Dir:  workspace, Stdin: token, Timeout: 10 * time.Second,
		})
		if err != nil || result.ExitCode != 0 {
			return nil, 0, false, commandFailure("spawn OpenClaw SSH server", result, err)
		}
		host, server := net.Pipe()
		go func() {
			defer server.Close()
			var one [1]byte
			_, _ = server.Read(one[:])
			sandbox.process.Store(false)
		}()
		return host, 12345, true, nil
	}
	return manager
}

func TestBridgeLifecycleReturnsPrivateEndpointAndRevokesConcurrently(t *testing.T) {
	sandbox := newFakeBridgeSandbox(t)
	manager := testManager(t, sandbox)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Address != "task-sandbox:2222" || endpoint.Network != "aries-net-test" || endpoint.ClientCommand != clientContainerPath || endpoint.IdentityFile != identityContainerPath {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	for path, mode := range map[string]os.FileMode{
		endpoint.ClientSourceFile: 0o555, endpoint.IdentitySourceFile: 0o600, endpoint.KnownHostsSourceFile: 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
			t.Fatalf("artifact %q = %v, %v", path, info, err)
		}
	}
	artifactDir := filepath.Dir(endpoint.IdentitySourceFile)
	const callers = 6
	errorsByCaller := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByCaller <- manager.Stop(ctx)
		}()
	}
	wait.Wait()
	close(errorsByCaller)
	for stopErr := range errorsByCaller {
		if stopErr != nil {
			t.Fatalf("Stop() error = %v", stopErr)
		}
	}
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
	if _, err := os.Lstat(artifactDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential artifact remains: %v", err)
	}
	if sandbox.process.Load() {
		t.Fatal("fake SSH server remains after Stop")
	}
	if sandbox.restarts.Load() != 1 {
		t.Fatalf("sandbox restarts = %d, want 1", sandbox.restarts.Load())
	}
	if got := strings.Join(sandbox.commandModes(), ","); got != "prepare,spawn,--verify-alias,--remove-file" {
		t.Fatalf("command modes = %q", got)
	}
}

func TestBridgeStartFailureRollsBackServerWorkspaceAndCredentials(t *testing.T) {
	sandbox := newFakeBridgeSandbox(t)
	manager := testManager(t, sandbox)
	probeFailure := errors.New("probe failed")
	manager.probe = func(context.Context, string, ssh.Signer, ssh.PublicKey) error { return probeFailure }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := manager.Start(ctx, sandbox)
	if !errors.Is(err, probeFailure) {
		t.Fatalf("Start() error = %v", err)
	}
	if sandbox.process.Load() {
		t.Fatal("server survived failed Start")
	}
	if got := strings.Join(sandbox.commandModes(), ","); got != "prepare,spawn,--recover-workspace,--remove-file" {
		t.Fatalf("rollback command modes = %q", got)
	}
	if sandbox.restarts.Load() != 1 {
		t.Fatalf("rollback restarts = %d, want 1", sandbox.restarts.Load())
	}
	if _, err := os.Lstat(filepath.Join(manager.outputDir, "bridges", "fixedid")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed Start credentials remain: %v", err)
	}
}

func TestBridgeStopCannotSucceedWithoutListenerAndOldKeyEvidence(t *testing.T) {
	sandbox := newFakeBridgeSandbox(t)
	manager := testManager(t, sandbox)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.Start(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	evidenceFailure := errors.New("listener evidence failed")
	manager.waitListener = func(context.Context, string) error { return evidenceFailure }
	if err := manager.Stop(ctx); !errors.Is(err, evidenceFailure) {
		t.Fatalf("Stop() error = %v", err)
	}
	manager.waitListener = func(context.Context, string) error { return nil }
	manager.oldKeyRejected = func(context.Context, *bridgeSession) error { return nil }
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("retry Stop() error = %v", err)
	}
}

func TestBridgePrepareLostResponseAndCancellationAlwaysUseTrustedRecovery(t *testing.T) {
	for _, injected := range []error{errors.New("lost prepare response"), context.Canceled} {
		sandbox := newFakeBridgeSandbox(t)
		sandbox.prepareErr = injected
		manager := testManager(t, sandbox)
		ctx, cancel := context.WithCancel(context.Background())
		if errors.Is(injected, context.Canceled) {
			cancel()
		} else {
			defer cancel()
		}
		_, err := manager.Start(ctx, sandbox)
		if !errors.Is(err, injected) {
			t.Fatalf("Start() error = %v, want %v", err, injected)
		}
		if !sandbox.recoverContextActive.Load() {
			t.Fatal("trusted workspace recovery did not receive a fresh live context")
		}
		if sandbox.restarts.Load() != 0 {
			t.Fatalf("prepare-only failure restarted sandbox %d times", sandbox.restarts.Load())
		}
		if got := strings.Join(sandbox.commandModes(), ","); got != "prepare,--recover-workspace,--remove-file" {
			t.Fatalf("prepare rollback modes = %q", got)
		}
		commands := sandbox.commandsSnapshot()
		if len(commands) < 2 || len(commands[0].Stdin) != workspaceOwnerTokenBytes || !bytes.Equal(commands[0].Stdin, commands[1].Stdin) {
			t.Fatalf("prepare/recovery ownership proof mismatch: %#v", commands)
		}
		for _, command := range commands[:2] {
			if bytes.Contains([]byte(strings.Join(command.Args, "\x00")), commands[0].Stdin) {
				t.Fatal("workspace ownership token leaked into command arguments")
			}
		}
	}
}

func TestBridgeForeignWorkspaceRootFailsClosedDuringUncertainPrepare(t *testing.T) {
	sandbox := newFakeBridgeSandbox(t)
	sandbox.prepareExit = workspaceRootExistsExit
	sandbox.recoveryErr = errors.New("workspace ownership marker missing")
	manager := testManager(t, sandbox)
	_, err := manager.Start(context.Background(), sandbox)
	if err == nil || !errors.Is(err, sandbox.recoveryErr) || !strings.Contains(err.Error(), "prepare OpenClaw SSH workspace") {
		t.Fatalf("Start() error = %v", err)
	}
	if got := strings.Join(sandbox.commandModes(), ","); got != "prepare,--recover-workspace,--remove-file" {
		t.Fatalf("foreign-root failure operations = %q", got)
	}
	if !sandbox.recoverContextActive.Load() {
		t.Fatal("uncertain prepare did not attempt proof-gated recovery")
	}
}

func TestBridgeLostPrepareResponseFailsClosedWhenOwnershipProofCannotBeValidated(t *testing.T) {
	sandbox := newFakeBridgeSandbox(t)
	sandbox.prepareErr = errors.New("lost prepare response")
	sandbox.recoveryErr = errors.New("workspace ownership marker mismatch")
	manager := testManager(t, sandbox)
	_, err := manager.Start(context.Background(), sandbox)
	if err == nil || !errors.Is(err, sandbox.prepareErr) || !errors.Is(err, sandbox.recoveryErr) {
		t.Fatalf("Start() error = %v", err)
	}
	if got := strings.Join(sandbox.commandModes(), ","); got != "prepare,--recover-workspace,--remove-file" {
		t.Fatalf("lost-response operations = %q", got)
	}
	if !sandbox.recoverContextActive.Load() {
		t.Fatal("lost prepare response did not attempt proof-gated recovery")
	}
}

func TestBridgeUncertainSpawnAlwaysRestartsWithFreshContext(t *testing.T) {
	sandbox := newFakeBridgeSandbox(t)
	manager := testManager(t, sandbox)
	spawnFailure := errors.New("spawn response lost")
	manager.startServer = func(context.Context, bridgeSandbox, string, string, string, []byte, []byte, []byte) (io.Closer, int, bool, error) {
		return nil, 0, false, spawnFailure
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := manager.Start(ctx, sandbox)
	if !errors.Is(err, spawnFailure) {
		t.Fatalf("Start() error = %v", err)
	}
	if sandbox.restarts.Load() != 1 {
		t.Fatalf("uncertain spawn restarts = %d, want 1", sandbox.restarts.Load())
	}
	if !sandbox.recoverContextActive.Load() {
		t.Fatal("uncertain spawn rollback did not use fresh cleanup context")
	}
}

func TestBridgeStopIgnoresExpiredGracefulContextOnlyWithinCleanupBound(t *testing.T) {
	sandbox := newFakeBridgeSandbox(t)
	manager := testManager(t, sandbox)
	if _, err := manager.Start(context.Background(), sandbox); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Stop(canceled); err != nil {
		t.Fatal(err)
	}
	if sandbox.restarts.Load() != 1 {
		t.Fatalf("Stop restarts = %d, want 1", sandbox.restarts.Load())
	}
}

func TestKeyBootstrapRequiresAuthenticatedPIDStillPresent(t *testing.T) {
	sandbox := newFakeBridgeSandbox(t)
	sandbox.process.Store(false)
	if err := requireProcessPresent(context.Background(), sandbox, 1234); err == nil {
		t.Fatal("missing authenticated PID was accepted before key bootstrap")
	}
	sandbox.process.Store(true)
	if err := requireProcessPresent(context.Background(), sandbox, 1234); err != nil {
		t.Fatal(err)
	}
	sandbox.processErr = errors.New("inspection failed")
	if err := requireProcessPresent(context.Background(), sandbox, 1234); !errors.Is(err, sandbox.processErr) {
		t.Fatalf("ProcessPresent error = %v", err)
	}
}
