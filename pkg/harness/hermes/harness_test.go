package hermes

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const testHermesImage = "docker.io/nousresearch/hermes-agent:v2026.5.29.2"

type fakeDocker struct {
	mu             sync.Mutex
	created        client.ContainerCreateOptions
	container      container.InspectResponse
	archive        []byte
	execs          map[string]client.ExecCreateOptions
	execRunning    map[string]bool
	execPresent    map[string]bool
	exitCodes      map[string]int
	agentStdout    string
	agentStderr    string
	agentExit      int
	sessionsStdout string
	sessionsExit   int
	removed        bool
	copyToErr      error
	logsErr        error
	inspectErr     error
	nextExec       int
	createCalls    int
	startCalls     int
	stopCalls      int
	killCalls      int
	removeCalls    int
	closeCalls     int
	closeErr       error
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		execs: make(map[string]client.ExecCreateOptions), execRunning: make(map[string]bool),
		execPresent: make(map[string]bool), exitCodes: make(map[string]int),
		agentStdout: "the task is complete\n", agentStderr: "hermes diagnostic\n",
		sessionsStdout: `{"role":"user","content":"do the task"}` + "\n",
	}
}

func (fake *fakeDocker) Close() error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.closeCalls++
	return fake.closeErr
}

func (fake *fakeDocker) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.createCalls++
	fake.created = options
	fake.container = container.InspectResponse{
		ID: "hermes-id", Config: options.Config, HostConfig: options.HostConfig, State: &container.State{},
	}
	return client.ContainerCreateResult{ID: "hermes-id"}, nil
}

func (fake *fakeDocker) CopyToContainer(_ context.Context, id string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if id != "hermes-id" || options.DestinationPath != "/" || !options.CopyUIDGID {
		return client.CopyToContainerResult{}, errors.New("unexpected copy request")
	}
	if fake.copyToErr != nil {
		return client.CopyToContainerResult{}, fake.copyToErr
	}
	content, err := io.ReadAll(options.Content)
	if err != nil {
		return client.CopyToContainerResult{}, err
	}
	fake.mu.Lock()
	fake.archive = content
	fake.mu.Unlock()
	return client.CopyToContainerResult{}, nil
}

func (fake *fakeDocker) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if id != fake.container.ID || fake.removed {
		return client.ContainerStartResult{}, errdefs.ErrNotFound
	}
	fake.startCalls++
	fake.container.State.Running = true
	return client.ContainerStartResult{}, nil
}

func (fake *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.removed || id != fake.container.ID {
		return client.ContainerInspectResult{}, fmt.Errorf("container absent: %w", errdefs.ErrNotFound)
	}
	if fake.inspectErr != nil {
		return client.ContainerInspectResult{}, fake.inspectErr
	}
	return client.ContainerInspectResult{Container: fake.container}, nil
}

func (fake *fakeDocker) ContainerTop(context.Context, string, client.ContainerTopOptions) (client.ContainerTopResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	processes := make([][]string, 0)
	for _, present := range fake.execPresent {
		if present {
			processes = append(processes, []string{"123"})
		}
	}
	return client.ContainerTopResult{Titles: []string{"PID"}, Processes: processes}, nil
}

func (fake *fakeDocker) ExecCreate(_ context.Context, id string, options client.ExecCreateOptions) (client.ExecCreateResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if id != fake.container.ID || !fake.container.State.Running {
		return client.ExecCreateResult{}, errdefs.ErrNotFound
	}
	fake.nextExec++
	execID := fmt.Sprintf("exec-%d", fake.nextExec)
	fake.execs[execID] = options
	fake.execRunning[execID] = true
	fake.execPresent[execID] = true
	return client.ExecCreateResult{ID: execID}, nil
}

func (fake *fakeDocker) ExecAttach(_ context.Context, execID string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	fake.mu.Lock()
	options, ok := fake.execs[execID]
	fake.mu.Unlock()
	if !ok {
		return client.ExecAttachResult{}, errdefs.ErrNotFound
	}
	clientSide, engineSide := net.Pipe()
	response := client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(clientSide, "application/vnd.docker.multiplexed-stream")}
	go func() {
		defer engineSide.Close()
		exitCode := 0
		// Cmd is /bin/sh -c <execShell> <label> <token> <command...>; index 5 is
		// the first token of the wrapped command.
		if len(options.Cmd) > 5 {
			switch options.Cmd[5] {
			case agentWrapperPath:
				_ = writeMux(engineSide, stdcopy.Stdout, []byte(fake.agentStdout))
				_ = writeMux(engineSide, stdcopy.Stderr, []byte(fake.agentStderr))
				exitCode = fake.agentExit
			case "hermes":
				_ = writeMux(engineSide, stdcopy.Stdout, []byte(fake.sessionsStdout))
				exitCode = fake.sessionsExit
			}
		}
		if len(options.Cmd) > 4 {
			trailer := fmt.Sprintf("\x1eARIES_HERMES_EXIT_%s=%d\x1f", options.Cmd[4], exitCode)
			_ = writeMux(engineSide, stdcopy.Stderr, []byte(trailer))
		}
		fake.mu.Lock()
		fake.exitCodes[execID] = exitCode
		fake.execPresent[execID] = false
		fake.execRunning[execID] = false
		fake.mu.Unlock()
	}()
	return response, nil
}

func (fake *fakeDocker) ExecInspect(_ context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	pid := 0
	if fake.execRunning[execID] {
		pid = 123
	}
	return client.ExecInspectResult{ID: execID, ContainerID: fake.container.ID, Running: fake.execRunning[execID], PID: pid, ExitCode: fake.exitCodes[execID]}, nil
}

func (fake *fakeDocker) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	fake.mu.Lock()
	err := fake.logsErr
	fake.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(multiplexed([]byte("container ready\n"), nil))), nil
}

func (fake *fakeDocker) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.stopCalls++
	fake.container.State.Running = false
	return client.ContainerStopResult{}, nil
}

func (fake *fakeDocker) ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.killCalls++
	fake.container.State.Running = false
	return client.ContainerKillResult{}, nil
}

func (fake *fakeDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.removeCalls++
	fake.removed = true
	return client.ContainerRemoveResult{}, nil
}

func multiplexed(stdout, stderr []byte) []byte {
	var content bytes.Buffer
	_ = writeMux(&content, stdcopy.Stdout, stdout)
	_ = writeMux(&content, stdcopy.Stderr, stderr)
	return content.Bytes()
}

func writeMux(writer io.Writer, stream stdcopy.StdType, content []byte) error {
	if len(content) == 0 {
		return nil
	}
	var header [8]byte
	header[0] = byte(stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(content)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(content)
	return err
}

func newTestManager(t *testing.T, fake *fakeDocker, secret []byte) *Manager {
	t.Helper()
	manager, err := New(Options{
		Image: testHermesImage, OutputDir: t.TempDir(), StartTimeout: 2 * time.Second, AgentTimeout: 2 * time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return secret, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.client = fake
	manager.newID = func() (string, error) { return "attempt", nil }
	return manager
}

func endpointFiles(t *testing.T) core.ToolEndpoint {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(path, []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.22.0.1:39425", Username: "aries", Network: "aries-net-test",
		IdentityFile: identityContainerFS, IdentitySourceFile: path,
	}
}

func testRequest(t *testing.T) core.HarnessRequest {
	t.Helper()
	return core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: validModel()}
}

func archiveEntries(t *testing.T, archive []byte) map[string]*tar.Header {
	t.Helper()
	entries := make(map[string]*tar.Header)
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		copied := *header
		entries[header.Name] = &copied
	}
	return entries
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	failure := errors.New("close failed")
	fake := newFakeDocker()
	fake.closeErr = failure
	manager := &Manager{client: fake}
	if err := manager.Close(); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if err := manager.Close(); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if fake.closeCalls != 1 {
		t.Fatalf("close calls = %d", fake.closeCalls)
	}
}

func TestStartStagesPrivateRuntimeAndPinsIdleContainer(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	config := fake.created.Config
	if !equalStrings(config.Entrypoint, idleEntrypoint) || !equalStrings(config.Cmd, idleCommand) {
		t.Fatalf("container is not pinned to the idle command: %v %v", config.Entrypoint, config.Cmd)
	}
	if config.Labels["aries.component"] != "harness" || config.Labels["aries.kind"] != "hermes-harness" || config.Labels["aries.managed"] != "true" {
		t.Fatalf("labels = %v", config.Labels)
	}
	if string(fake.created.HostConfig.NetworkMode) != "aries-net-test" {
		t.Fatalf("network = %v", fake.created.HostConfig.NetworkMode)
	}

	entries := archiveEntries(t, fake.archive)
	for name, mode := range map[string]int64{
		strings.TrimPrefix(configContainerPath, "/"): 0o600,
		strings.TrimPrefix(modelKeyPath, "/"):        0o600,
		strings.TrimPrefix(identityContainerFS, "/"): 0o600,
		strings.TrimPrefix(agentWrapperPath, "/"):    0o555,
	} {
		header, ok := entries[name]
		if !ok {
			t.Fatalf("staged archive is missing %s", name)
		}
		if header.Mode != mode {
			t.Fatalf("%s mode = %o, want %o", name, header.Mode, mode)
		}
	}
	// Every staged entry must be owned by the image's unprivileged `hermes`
	// user. The PATH shim drops root to that UID before exec'ing the real
	// binary, so root-owned staging leaves Hermes unable to read its own
	// configuration — and the failure appears only once the agent runs.
	for name, header := range entries {
		if header.Uid != runtimeUID || header.Gid != runtimeGID {
			t.Fatalf("%s owned by %d:%d, want %d:%d", name, header.Uid, header.Gid, runtimeUID, runtimeGID)
		}
	}
}

// The credential may live only in the staged key file, never in Docker's
// container configuration, environment, labels, or the retained config.
func TestStartKeepsCredentialOutOfDockerMetadataAndArtifacts(t *testing.T) {
	secret := []byte("sk-super-secret-value")
	fake := newFakeDocker()
	manager := newTestManager(t, fake, secret)
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	config := fake.created.Config
	for _, value := range append(append([]string(nil), config.Env...), append(config.Cmd, config.Entrypoint...)...) {
		if strings.Contains(value, string(secret)) {
			t.Fatalf("secret leaked into Docker configuration: %q", value)
		}
	}
	for name, value := range config.Labels {
		if strings.Contains(value, string(secret)) {
			t.Fatalf("secret leaked into label %s", name)
		}
	}
	retained, err := os.ReadFile(filepath.Join(manager.outputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(retained, secret) {
		t.Fatalf("secret leaked into the retained config:\n%s", retained)
	}
	if !bytes.Contains(retained, []byte("${DEEPSEEK_API_KEY}")) {
		t.Fatalf("retained config lost its credential reference:\n%s", retained)
	}
	entries := archiveEntries(t, fake.archive)
	if entries[strings.TrimPrefix(modelKeyPath, "/")].Size != int64(len(secret)) {
		t.Fatal("staged key file does not carry the credential")
	}
}

func TestRunReturnsFinalResponseAndArtifacts(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "fix the git repository")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != core.StatusSucceeded || result.FinalResponse != "the task is complete" {
		t.Fatalf("result = %#v", result)
	}
	artifacts := filepath.Join(manager.outputDir, request.TaskID, "harness")
	for _, name := range []string{"config.yaml", "hermes_stdout.log", "hermes_stderr.log", "container.log", "telemetry.index.json", filepath.Join("telemetry", "sessions.jsonl")} {
		if _, err := os.Stat(filepath.Join(artifacts, name)); err != nil {
			t.Fatalf("missing artifact %s: %v", name, err)
		}
	}
	if len(result.LogPaths) == 0 {
		t.Fatal("result carries no log paths")
	}
	// The instruction must reach Hermes as one argv element, not a shell string.
	var agentCmd []string
	for _, options := range fake.execs {
		if len(options.Cmd) > 5 && options.Cmd[5] == agentWrapperPath {
			agentCmd = options.Cmd
		}
	}
	if len(agentCmd) != 9 || agentCmd[6] != "deepseek-v4-flash" || agentCmd[7] != "deepseek" || agentCmd[8] != "fix the git repository" {
		t.Fatalf("agent exec argv = %#v", agentCmd)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunAcceptsExactlyOneInstruction(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if _, err := manager.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), "second"); err == nil {
		t.Fatal("second instruction was accepted")
	}
}

func TestRunRejectsInvalidInstructionAndUnstartedHarness(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	if _, err := manager.Run(context.Background(), "task"); err == nil {
		t.Fatal("run before start was accepted")
	}
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	for _, instruction := range []string{"", "   ", "has\x00nul"} {
		if _, err := manager.Run(context.Background(), instruction); err == nil {
			t.Fatalf("instruction %q was accepted", instruction)
		}
	}
}

// A non-zero Hermes exit is a harness failure, but the artifacts collected on
// the way out must still be retained for diagnosis.
func TestRunReportsNonZeroExitAndStillRetainsArtifacts(t *testing.T) {
	fake := newFakeDocker()
	fake.agentExit = 1
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "exited with status 1") {
		t.Fatalf("err = %v", err)
	}
	if result.Status != core.StatusFailed {
		t.Fatalf("status = %q", result.Status)
	}
	if _, statErr := os.Stat(filepath.Join(manager.outputDir, request.TaskID, "harness", "hermes_stderr.log")); statErr != nil {
		t.Fatalf("stderr artifact missing after failure: %v", statErr)
	}
}

func TestRunCancellationIsReportedAsCanceled(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := manager.Run(ctx, "task")
	if err == nil {
		t.Fatal("canceled run reported success")
	}
	if result.Status != core.StatusCanceled {
		t.Fatalf("status = %q, want %q", result.Status, core.StatusCanceled)
	}
}

// A run that never reached the model produces no session; that must not be
// reported as a harness failure.
func TestEmptySessionExportIsNotAFailure(t *testing.T) {
	fake := newFakeDocker()
	fake.sessionsStdout = ""
	fake.sessionsExit = 1
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("empty session export failed the run: %v", err)
	}
	if result.Status != core.StatusSucceeded {
		t.Fatalf("status = %q", result.Status)
	}
	if _, statErr := os.Stat(filepath.Join(manager.outputDir, request.TaskID, "harness", "telemetry", "sessions.jsonl")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected sessions artifact: %v", statErr)
	}
}

func TestStopIsIdempotentAndConfirmsAbsence(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	for attempt := range 3 {
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatalf("stop %d: %v", attempt, err)
		}
	}
	if fake.removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1", fake.removeCalls)
	}
	if !fake.removed {
		t.Fatal("container was not removed")
	}
	if manager.active != nil {
		t.Fatal("manager still holds an active session after stop")
	}
}

// Positive absence: if the container is still inspectable after removal, Stop
// must fail rather than report a clean teardown.
func TestStopFailsWhenContainerRemains(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.removeCalls = 0
	fake.mu.Unlock()
	// Removal silently does nothing, so the container stays present.
	stubborn := &stubbornDocker{fakeDocker: fake}
	manager.client = stubborn
	if err := manager.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "remains after removal") {
		t.Fatalf("err = %v", err)
	}
	if manager.active == nil {
		t.Fatal("failed stop must keep the session for a later retry")
	}
}

type stubbornDocker struct {
	*fakeDocker
}

func (stubborn *stubbornDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	stubborn.mu.Lock()
	defer stubborn.mu.Unlock()
	stubborn.removeCalls++
	return client.ContainerRemoveResult{}, nil
}

func TestStartRollsBackAndClearsArtifactsOnFailure(t *testing.T) {
	fake := newFakeDocker()
	fake.copyToErr = errors.New("copy refused")
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "copy private Hermes runtime") {
		t.Fatalf("err = %v", err)
	}
	if !fake.removed {
		t.Fatal("partial container was not removed")
	}
	if manager.active != nil {
		t.Fatal("manager kept an active session after a clean rollback")
	}
	if _, err := os.Stat(filepath.Join(manager.outputDir, request.TaskID, "harness")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact directory survived rollback: %v", err)
	}
}

func TestStartRejectsSecondSession(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if err := manager.Start(context.Background(), testRequest(t)); err == nil {
		t.Fatal("second Start was accepted")
	}
}

func TestStartValidatesIdentifiersAndResources(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*core.HarnessRequest)
		want string
	}{
		{"run id", func(r *core.HarnessRequest) { r.RunID = "-bad" }, "Hermes run ID contains an unsafe character"},
		{"task id", func(r *core.HarnessRequest) { r.TaskID = "" }, "Hermes task ID is invalid"},
		{"timeout", func(r *core.HarnessRequest) { r.Timeout = -1 }, "Hermes task timeout must not be negative"},
		{"cpu", func(r *core.HarnessRequest) { value := math.Exp2(63) / 1e9; r.CPU = &value }, "Hermes CPU must be finite, positive, and convert to NanoCPUs below 2^63"},
		{"memory", func(r *core.HarnessRequest) { value := int(math.MaxInt64>>20) + 1; r.MemoryMB = &value }, fmt.Sprintf("Hermes memory must be positive and no greater than %d MiB", int64(math.MaxInt64)>>20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := newTestManager(t, fake, []byte("model-secret"))
			request := testRequest(t)
			test.set(&request)
			if err := manager.Start(context.Background(), request); err == nil || err.Error() != test.want {
				t.Fatalf("err = %v, want exactly %q", err, test.want)
			}
			if fake.createCalls != 0 {
				t.Fatalf("ContainerCreate calls = %d, want zero", fake.createCalls)
			}
		})
	}
}

func TestStartRequiresPresentCredential(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, nil)
	manager.apiKeyLookup = func(string) ([]byte, bool) { return nil, false }
	if err := manager.Start(context.Background(), testRequest(t)); err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("err = %v", err)
	}
	manager.apiKeyLookup = func(string) ([]byte, bool) { return []byte("has\nnewline"), true }
	if err := manager.Start(context.Background(), testRequest(t)); err == nil || !strings.Contains(err.Error(), "NUL or a line break") {
		t.Fatalf("err = %v", err)
	}
	if fake.createCalls != 0 {
		t.Fatalf("ContainerCreate calls = %d, want zero", fake.createCalls)
	}
}

// The upstream image declares VOLUME /opt/data, which Docker always
// materialises. That one anonymous volume is tolerated; a bind mount, which is
// how host state would reach the harness, is not.
func TestValidateContainerAllowsOnlyTheImageDeclaredVolume(t *testing.T) {
	for _, test := range []struct {
		name    string
		mounts  []container.MountPoint
		binds   []string
		wantErr bool
	}{
		{name: "no mounts"},
		{name: "image volume", mounts: []container.MountPoint{{Type: "volume", Name: "anon", Destination: imageDeclaredVolume}}},
		{name: "bind mount", mounts: []container.MountPoint{{Type: "bind", Source: "/etc", Destination: "/etc"}}, wantErr: true},
		{name: "unnamed volume", mounts: []container.MountPoint{{Type: "volume", Destination: imageDeclaredVolume}}, wantErr: true},
		{name: "other destination", mounts: []container.MountPoint{{Type: "volume", Name: "anon", Destination: "/workspace"}}, wantErr: true},
		{name: "host bind request", binds: []string{"/etc:/etc"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := newTestManager(t, fake, []byte("model-secret"))
			request := testRequest(t)
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			defer manager.Stop(context.Background())
			fake.mu.Lock()
			fake.container.Mounts = test.mounts
			fake.container.HostConfig.Binds = test.binds
			fake.mu.Unlock()
			err := manager.validateContainer(context.Background(), manager.active)
			if test.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestNewRejectsUnpinnedImage(t *testing.T) {
	for _, image := range []string{"", "nousresearch/hermes-agent", "nousresearch/hermes-agent:latest"} {
		if _, err := New(Options{Image: image, OutputDir: t.TempDir()}); err == nil {
			t.Fatalf("image %q was accepted", image)
		}
	}
}
