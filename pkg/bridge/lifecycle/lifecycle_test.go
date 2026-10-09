package lifecycle

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

type fakeSandbox struct {
	mu            sync.Mutex
	running       bool
	operations    []string
	checkpointErr error
	restoreErr    error
	// execGate, when set, holds each command until closed.
	execGate chan struct{}
	started  chan struct{}
}

func (f *fakeSandbox) record(operation string) {
	f.mu.Lock()
	f.operations = append(f.operations, operation)
	f.mu.Unlock()
}

func (f *fakeSandbox) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.operations)
}

func (f *fakeSandbox) Checkpoint(context.Context) error {
	f.record("checkpoint")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkpointErr != nil {
		return f.checkpointErr
	}
	f.running = false
	return nil
}

func (f *fakeSandbox) Restore(context.Context) error {
	f.record("restore")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.running = true
	return nil
}

func (f *fakeSandbox) ExecStream(ctx context.Context, command core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
	f.mu.Lock()
	running, gate, started := f.running, f.execGate, f.started
	f.mu.Unlock()
	if !running {
		return core.CommandResult{ExitCode: -1}, errors.New("exec in a stopped sandbox")
	}
	f.record("exec " + command.Path)
	if started != nil {
		started <- struct{}{}
	}
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return core.CommandResult{ExitCode: -1}, ctx.Err()
		}
	}
	return core.CommandResult{ExitCode: 3}, nil
}

func (f *fakeSandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	return f.ExecStream(ctx, command, nil, nil, nil)
}

func (f *fakeSandbox) Upload(context.Context, string, string) error {
	f.record("upload")
	return nil
}
func (f *fakeSandbox) Download(context.Context, string, string) error {
	f.record("download")
	return nil
}
func (f *fakeSandbox) DownloadLimit(context.Context, string, string, int64) error {
	f.record("download-limit")
	return nil
}
func (f *fakeSandbox) Connectivity() core.HarnessConnectivity {
	return core.HarnessConnectivity{Placement: core.RuntimePlacement{DockerNetwork: "task-network"}}
}
func (f *fakeSandbox) ContainerID() string   { return "container-id" }
func (f *fakeSandbox) ContainerName() string { return "aries-task-x" }
func (f *fakeSandbox) RunID() string         { return "run" }
func (f *fakeSandbox) TaskID() string        { return "task" }
func (f *fakeSandbox) Workdir() string       { return "/work" }

// plainSandbox lacks the checkpoint capability.
type plainSandbox struct{}

func (plainSandbox) Connectivity() core.HarnessConnectivity { return core.HarnessConnectivity{} }
func (plainSandbox) Exec(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, nil
}
func (plainSandbox) Upload(context.Context, string, string) error   { return nil }
func (plainSandbox) Download(context.Context, string, string) error { return nil }

type fakeToolSandbox struct {
	live     runner.Sandbox
	starts   int
	stops    int
	stopErr  error
	requests []core.SandboxRequest
}

func (f *fakeToolSandbox) Start(_ context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	f.starts++
	f.requests = append(f.requests, request)
	return f.live, nil
}

func (f *fakeToolSandbox) Stop(_ context.Context, live runner.Sandbox) error {
	if live != f.live {
		return errors.New("stop of a foreign sandbox")
	}
	f.stops++
	return f.stopErr
}

type fakeAccess struct {
	granted  runner.Sandbox
	starts   int
	stops    int
	startErr error
	stopErr  error
}

func (f *fakeAccess) Start(_ context.Context, live runner.Sandbox) (core.ToolEndpoint, error) {
	f.starts++
	f.granted = live
	return core.ToolEndpoint{Protocol: "ssh", LogPaths: []string{"/runs/task/bridge/tool-calls.jsonl"}}, f.startErr
}

func (f *fakeAccess) Stop(context.Context) error {
	f.stops++
	return f.stopErr
}

func request() core.SandboxRequest {
	return core.SandboxRequest{RunID: "run", TaskID: "task", Environment: core.Environment{Image: "image", Workdir: "/work"}}
}

func newBridge(t *testing.T, mode Mode, live runner.Sandbox) (*Bridge, *fakeToolSandbox, *fakeAccess, string) {
	t.Helper()
	toolSandbox := &fakeToolSandbox{live: live}
	access := &fakeAccess{}
	output := t.TempDir()
	bridge, err := New(Options{ToolSandbox: toolSandbox, Access: access, Mode: mode, OutputDir: output, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return bridge, toolSandbox, access, output
}

// waitIdle waits for the checkpoint scheduled after the last operation.
func waitIdle(t *testing.T, bridge *Bridge) {
	t.Helper()
	bridge.mu.Lock()
	suspending := bridge.suspending
	bridge.mu.Unlock()
	suspending.pending.Wait()
}

func readEvents(t *testing.T, path string) []checkpointEvent {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []checkpointEvent
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event checkpointEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestParseMode(t *testing.T) {
	for value, want := range map[string]Mode{"": Persistent, "persistent": Persistent, "checkpoint": Checkpoint} {
		if got, err := ParseMode(value); err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %q, %v", value, got, err)
		}
	}
	if _, err := ParseMode("restore"); err == nil {
		t.Fatal("ParseMode accepted an unknown mode")
	}
}

func TestNewRejectsMissingDependencies(t *testing.T) {
	for name, options := range map[string]Options{
		"sandbox":  {Access: &fakeAccess{}},
		"access":   {ToolSandbox: &fakeToolSandbox{}},
		"mode":     {ToolSandbox: &fakeToolSandbox{}, Access: &fakeAccess{}, Mode: "restore"},
		"evidence": {ToolSandbox: &fakeToolSandbox{}, Access: &fakeAccess{}, Mode: Checkpoint},
	} {
		if _, err := New(options); err == nil {
			t.Fatalf("New() without %s error = nil", name)
		}
	}
}

func TestPersistentBridgeExposesTheOwnedSandboxUnchanged(t *testing.T) {
	live := &fakeSandbox{running: true}
	bridge, toolSandbox, access, output := newBridge(t, Persistent, live)
	ctx := context.Background()
	opened, err := bridge.Open(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if opened != live {
		t.Fatalf("Open() = %T, want the tool sandbox's own capability", opened)
	}
	if _, err := bridge.Open(ctx, request()); err == nil {
		t.Fatal("second Open() error = nil")
	}
	endpoint, err := bridge.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if access.granted != live || !reflect.DeepEqual(endpoint.LogPaths, []string{"/runs/task/bridge/tool-calls.jsonl"}) {
		t.Fatalf("granted %T with log paths %v", access.granted, endpoint.LogPaths)
	}
	if _, err := opened.Exec(ctx, core.Command{Path: "/bin/true"}); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := bridge.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if toolSandbox.starts != 1 || toolSandbox.stops != 1 || access.stops != 1 {
		t.Fatalf("sandbox starts=%d stops=%d access stops=%d", toolSandbox.starts, toolSandbox.stops, access.stops)
	}
	if got := live.snapshot(); !reflect.DeepEqual(got, []string{"exec /bin/true"}) {
		t.Fatalf("operations = %v, want no checkpoint", got)
	}
	if _, err := os.Stat(filepath.Join(output, "task", eventsDirectory)); !os.IsNotExist(err) {
		t.Fatalf("persistent mode wrote checkpoint evidence: %v", err)
	}
}

func TestCheckpointBridgeSuspendsBetweenToolCallsOnlyWhileGranted(t *testing.T) {
	live := &fakeSandbox{running: true}
	bridge, toolSandbox, access, output := newBridge(t, Checkpoint, live)
	ctx := context.Background()
	opened, err := bridge.Open(ctx, request())
	if err != nil {
		t.Fatal(err)
	}
	if opened == runner.Sandbox(live) {
		t.Fatal("checkpoint mode exposed the unwrapped sandbox")
	}
	for _, capability := range []bool{
		func() bool { _, ok := opened.(runner.StreamExecutor); return ok }(),
		func() bool { _, ok := opened.(runner.LimitedDownloader); return ok }(),
	} {
		if !capability {
			t.Fatal("checkpoint mode hid a sandbox capability")
		}
	}
	// Preparation runs against the live sandbox without checkpoints.
	if err := opened.Upload(ctx, "/host/file", "/work/file"); err != nil {
		t.Fatal(err)
	}
	endpoint, err := bridge.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eventsPath := filepath.Join(output, "task", eventsDirectory, eventsFileName)
	if access.granted != opened || !slices.Contains(endpoint.LogPaths, eventsPath) {
		t.Fatalf("granted %T with log paths %v", access.granted, endpoint.LogPaths)
	}
	stream := access.granted.(runner.StreamExecutor)
	for _, path := range []string{"/bin/a", "/bin/b"} {
		result, err := stream.ExecStream(ctx, core.Command{Path: path}, nil, io.Discard, io.Discard)
		if err != nil || result.ExitCode != 3 {
			t.Fatalf("ExecStream(%s) = %#v, %v", path, result, err)
		}
		waitIdle(t, bridge)
	}
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// Evaluation restores once and leaves the sandbox running.
	for _, path := range []string{"/bin/verify", "/bin/score"} {
		if _, err := opened.Exec(ctx, core.Command{Path: path}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bridge.Close(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"upload", "checkpoint",
		"restore", "exec /bin/a", "checkpoint",
		"restore", "exec /bin/b", "checkpoint",
		"restore", "exec /bin/verify", "exec /bin/score",
	}
	if got := live.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}
	if toolSandbox.stops != 1 {
		t.Fatalf("sandbox stops = %d", toolSandbox.stops)
	}
	events := readEvents(t, eventsPath)
	var kinds []string
	for index, event := range events {
		if event.Sequence != uint64(index+1) || event.Status != core.StatusSucceeded || event.TaskID != "task" || event.Container != "container-id" {
			t.Fatalf("event %d = %#v", index, event)
		}
		kinds = append(kinds, event.Event)
	}
	wantKinds := []string{"checkpoint", "restore", "checkpoint", "restore", "checkpoint", "restore"}
	if !reflect.DeepEqual(kinds, wantKinds) || events[len(events)-1].Granted {
		t.Fatalf("events = %#v, want %v ending with an ungranted restore", events, wantKinds)
	}
}

func TestCheckpointWaitsForOverlappingToolCalls(t *testing.T) {
	live := &fakeSandbox{running: true, execGate: make(chan struct{}), started: make(chan struct{}, 2)}
	bridge, _, access, _ := newBridge(t, Checkpoint, live)
	ctx := context.Background()
	if _, err := bridge.Open(ctx, request()); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Start(ctx); err != nil {
		t.Fatal(err)
	}
	stream := access.granted.(runner.StreamExecutor)
	var wait sync.WaitGroup
	for _, path := range []string{"/bin/a", "/bin/b"} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := stream.ExecStream(ctx, core.Command{Path: path}, nil, io.Discard, io.Discard); err != nil {
				t.Error(err)
			}
		}()
	}
	<-live.started
	<-live.started
	close(live.execGate)
	wait.Wait()
	waitIdle(t, bridge)
	got := live.snapshot()
	if len(got) != 5 || got[0] != "checkpoint" || got[1] != "restore" || got[4] != "checkpoint" {
		t.Fatalf("operations = %v, want one restore and one checkpoint around both calls", got)
	}
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointModeRejectsTasksItCannotSuspendBeforeAllocation(t *testing.T) {
	for name, mutate := range map[string]func(*core.SandboxRequest){
		"service": func(r *core.SandboxRequest) { r.Environment.Services.SearchPort = 8080 },
		"gpu":     func(r *core.SandboxRequest) { r.Environment.GPUs = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			bridge, toolSandbox, _, _ := newBridge(t, Checkpoint, &fakeSandbox{running: true})
			req := request()
			mutate(&req)
			if _, err := bridge.Open(context.Background(), req); err == nil {
				t.Fatal("Open() error = nil")
			}
			if toolSandbox.starts != 0 {
				t.Fatal("rejected task allocated a sandbox")
			}
		})
	}
}

func TestCheckpointModeRequiresCapabilityAndCloseStillRemovesSandbox(t *testing.T) {
	bridge, toolSandbox, _, _ := newBridge(t, Checkpoint, plainSandbox{})
	ctx := context.Background()
	if _, err := bridge.Open(ctx, request()); err == nil || !strings.Contains(err.Error(), "requires a checkpointing") {
		t.Fatalf("Open() error = %v", err)
	}
	if err := bridge.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if toolSandbox.stops != 1 {
		t.Fatalf("sandbox stops = %d, want the failed open's sandbox removed", toolSandbox.stops)
	}
}

func TestFailedGrantCheckpointFailsStartAndRevocation(t *testing.T) {
	live := &fakeSandbox{running: true, checkpointErr: errors.New("criu dump failed")}
	bridge, _, access, _ := newBridge(t, Checkpoint, live)
	ctx := context.Background()
	if _, err := bridge.Open(ctx, request()); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Start(ctx); !errors.Is(err, live.checkpointErr) {
		t.Fatalf("Start() error = %v", err)
	}
	if access.starts != 0 {
		t.Fatal("access granted after a failed checkpoint")
	}
	if err := bridge.Stop(ctx); !errors.Is(err, live.checkpointErr) {
		t.Fatalf("Stop() error = %v, want the checkpoint failure", err)
	}
	if err := bridge.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFailedToolCallCheckpointIsRecordedAndFailsRevocation(t *testing.T) {
	live := &fakeSandbox{running: true}
	bridge, _, access, output := newBridge(t, Checkpoint, live)
	ctx := context.Background()
	if _, err := bridge.Open(ctx, request()); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Start(ctx); err != nil {
		t.Fatal(err)
	}
	live.mu.Lock()
	live.checkpointErr = errors.New("criu dump failed")
	live.mu.Unlock()
	stream := access.granted.(runner.StreamExecutor)
	result, err := stream.ExecStream(ctx, core.Command{Path: "/bin/a"}, nil, io.Discard, io.Discard)
	if err != nil || result.ExitCode != 3 {
		t.Fatalf("ExecStream() = %#v, %v; the command result must not absorb a later checkpoint failure", result, err)
	}
	if err := bridge.Stop(ctx); !errors.Is(err, live.checkpointErr) {
		t.Fatalf("Stop() error = %v, want the checkpoint failure", err)
	}
	if err := bridge.Close(ctx); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, filepath.Join(output, "task", eventsDirectory, eventsFileName))
	last := events[len(events)-1]
	if last.Event != "checkpoint" || last.Status != core.StatusFailed || !strings.Contains(last.Error, "criu dump failed") {
		t.Fatalf("last event = %#v", last)
	}
}

func TestFailedRestoreFailsTheToolCallWithoutRunningIt(t *testing.T) {
	live := &fakeSandbox{running: true}
	bridge, _, access, _ := newBridge(t, Checkpoint, live)
	ctx := context.Background()
	if _, err := bridge.Open(ctx, request()); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Start(ctx); err != nil {
		t.Fatal(err)
	}
	live.mu.Lock()
	live.restoreErr = errors.New("criu restore failed")
	live.mu.Unlock()
	stream := access.granted.(runner.StreamExecutor)
	if _, err := stream.ExecStream(ctx, core.Command{Path: "/bin/a"}, nil, io.Discard, io.Discard); !errors.Is(err, live.restoreErr) {
		t.Fatalf("ExecStream() error = %v", err)
	}
	live.mu.Lock()
	live.restoreErr = nil
	live.mu.Unlock()
	if _, err := stream.ExecStream(ctx, core.Command{Path: "/bin/b"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("ExecStream() after a transient restore failure error = %v", err)
	}
	waitIdle(t, bridge)
	if got := live.snapshot(); !reflect.DeepEqual(got, []string{"checkpoint", "restore", "restore", "exec /bin/b", "checkpoint"}) {
		t.Fatalf("operations = %v", got)
	}
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestStopReturnsAccessFailureBeforeEndingCheckpoints(t *testing.T) {
	live := &fakeSandbox{running: true}
	bridge, _, access, _ := newBridge(t, Checkpoint, live)
	ctx := context.Background()
	if _, err := bridge.Open(ctx, request()); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Start(ctx); err != nil {
		t.Fatal(err)
	}
	access.stopErr = errors.New("commands still active")
	if err := bridge.Stop(ctx); !errors.Is(err, access.stopErr) {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := bridge.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
