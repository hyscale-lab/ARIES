package openclaw

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

const testOpenClawImage = "example.invalid/openclaw:fixture@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

type fakeDocker struct {
	mu               sync.Mutex
	created          client.ContainerCreateOptions
	container        container.InspectResponse
	archive          []byte
	execs            map[string]client.ExecCreateOptions
	exitCodes        map[string]int
	execRunning      map[string]bool
	execPresent      map[string]bool
	removed          bool
	copyToErr        error
	containerLogsErr error
	copyFromErr      error
	inspectErr       error
	stopErr          error
	killErr          error
	removeErr        error
	keepAttachOpen   bool
	nextExec         int
	startCalls       int
	logsCalls        int
	stopCalls        int
	killCalls        int
	removeCalls      int
	createCalls      int
}

func TestHarnessAppliesOnlyPresentCheckedResources(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	cpu, memory := 2.5, 1536
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), CPU: &cpu, MemoryMB: &memory}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := fake.created.HostConfig.Resources; got.NanoCPUs != 2_500_000_000 || got.Memory != 1536<<20 {
		t.Fatalf("resources = %#v", got)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessRejectsInvalidResourcesBeforeContainerCreate(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*core.HarnessRequest)
		want string
	}{
		{"cpu", func(request *core.HarnessRequest) { request.CPU = floatPointer(math.Exp2(63) / 1e9) }, "OpenClaw CPU must be finite, positive, and convert to NanoCPUs below 2^63"},
		{"memory", func(request *core.HarnessRequest) { request.MemoryMB = intPointer(int(math.MaxInt64>>20) + 1) }, fmt.Sprintf("OpenClaw memory must be positive and no greater than %d MiB", int64(math.MaxInt64)>>20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := newTestManager(t, fake, []byte("model-secret"))
			request := core.HarnessRequest{
				RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(),
			}
			test.set(&request)
			if err := manager.Start(context.Background(), request); err == nil || err.Error() != test.want {
				t.Fatalf("Start() error = %v, want exactly %q", err, test.want)
			}
			if fake.createCalls != 0 {
				t.Fatalf("ContainerCreate calls = %d, want zero", fake.createCalls)
			}
		})
	}
}

func floatPointer(value float64) *float64 { return &value }
func intPointer(value int) *int           { return &value }

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		execs: make(map[string]client.ExecCreateOptions), exitCodes: make(map[string]int),
		execRunning: make(map[string]bool), execPresent: make(map[string]bool),
	}
}

func (fake *fakeDocker) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.createCalls++
	fake.created = options
	fake.container = container.InspectResponse{
		ID: "openclaw-id", Config: options.Config, HostConfig: options.HostConfig,
		State: &container.State{},
	}
	return client.ContainerCreateResult{ID: "openclaw-id"}, nil
}

func (fake *fakeDocker) CopyToContainer(_ context.Context, id string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if id != "openclaw-id" || options.DestinationPath != "/" || !options.CopyUIDGID {
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
		if len(options.Cmd) > 5 && options.Cmd[5] == launcherPath {
			_ = writeMux(engineSide, stdcopy.Stdout, []byte(`{"status":"ok","result":{"payloads":[{"text":"task complete"}]}}`))
			_ = writeMux(engineSide, stdcopy.Stderr, []byte("agent diagnostic\n"))
		}
		if len(options.Cmd) > 4 {
			trailer := fmt.Sprintf("\x1eARIES_OPENCLAW_EXIT_%s=%d\x1f", options.Cmd[4], exitCode)
			_ = writeMux(engineSide, stdcopy.Stderr, []byte(trailer))
		}
		fake.mu.Lock()
		fake.exitCodes[execID] = exitCode
		fake.execPresent[execID] = false
		if !fake.keepAttachOpen {
			fake.execRunning[execID] = false
		}
		fake.mu.Unlock()
		if fake.keepAttachOpen {
			_, _ = io.Copy(io.Discard, engineSide)
			fake.mu.Lock()
			fake.execRunning[execID] = false
			fake.mu.Unlock()
		}
	}()
	return response, nil
}

func (fake *fakeDocker) ExecInspect(_ context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	status := fake.exitCodes[execID]
	pid := 0
	if fake.execRunning[execID] {
		pid = 123
	}
	return client.ExecInspectResult{ID: execID, ContainerID: fake.container.ID, Running: fake.execRunning[execID], PID: pid, ExitCode: status}, nil
}

func (fake *fakeDocker) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	fake.mu.Lock()
	fake.logsCalls++
	err := fake.containerLogsErr
	fake.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(multiplexed([]byte("gateway ready\n"), nil))), nil
}

func (fake *fakeDocker) CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	if fake.copyFromErr != nil {
		return client.CopyFromContainerResult{}, fake.copyFromErr
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	content := []byte("{\"event\":\"tool\"}\n")
	_ = writer.WriteHeader(&tar.Header{Name: "sessions/run.trajectory.jsonl", Mode: 0o600, Size: int64(len(content))})
	_, _ = writer.Write(content)
	_ = writer.Close()
	return client.CopyFromContainerResult{Content: io.NopCloser(bytes.NewReader(archive.Bytes()))}, nil
}

func TestCollectTelemetryIgnoresOnlyTypedMissingPath(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	active := &session{containerID: "openclaw-id", artifactDir: t.TempDir()}

	fake.copyFromErr = fmt.Errorf("session path absent: %w", errdefs.ErrNotFound)
	if paths, err := manager.collectTelemetry(context.Background(), active); err != nil || len(paths) != 0 {
		t.Fatalf("typed missing telemetry = %v, %v", paths, err)
	}

	fake.copyFromErr = errors.New("daemon not found while unavailable")
	if _, err := manager.collectTelemetry(context.Background(), active); err == nil || !strings.Contains(err.Error(), "daemon not found") {
		t.Fatalf("untyped daemon failure was masked: %v", err)
	}
}

func (fake *fakeDocker) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.stopCalls++
	if fake.stopErr != nil {
		return client.ContainerStopResult{}, fake.stopErr
	}
	fake.container.State.Running = false
	return client.ContainerStopResult{}, nil
}

func (fake *fakeDocker) ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.killCalls++
	if fake.killErr != nil {
		return client.ContainerKillResult{}, fake.killErr
	}
	fake.container.State.Running = false
	return client.ContainerKillResult{}, nil
}

func (fake *fakeDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.removeCalls++
	if fake.removeErr != nil {
		return client.ContainerRemoveResult{}, fake.removeErr
	}
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
		Image: testOpenClawImage, OutputDir: t.TempDir(), StartTimeout: time.Second, AgentTimeout: time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return secret, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.client = fake
	manager.newID = func() (string, error) { return "attempt", nil }
	return manager
}

func TestNewRequiresImmutableConfiguredImage(t *testing.T) {
	for _, image := range []string{"", "example.invalid/openclaw:latest", "example.invalid/openclaw@sha256:short"} {
		if _, err := New(Options{Image: image, OutputDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "OpenClaw image") {
			t.Fatalf("New(%q) error = %v", image, err)
		}
	}
}

func endpointFiles(t *testing.T) core.ToolEndpoint {
	t.Helper()
	root := t.TempDir()
	write := func(name string, mode os.FileMode, content string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.22.0.1:39425", Username: "aries", Network: "aries-net-test",
		ClientCommand: "/opt/aries/bin/aries-ssh", ClientSourceFile: write("aries-ssh", 0o555, "client"),
		IdentityFile: "/run/aries/ssh/id_ed25519", IdentitySourceFile: write("id_ed25519", 0o600, "identity"),
		KnownHostsFile: "/run/aries/ssh/known_hosts", KnownHostsSourceFile: write("known_hosts", 0o600, "known"),
	}
}

func TestHarnessUsesOneSDKContainerAndDirectPrivateArchive(t *testing.T) {
	fake := newFakeDocker()
	// Docker 29 may keep the hijacked attach socket open after ExecInspect says
	// the process exited. The harness must still drain and return exact output.
	fake.keepAttachOpen = true
	secret := []byte("model-secret")
	manager := newTestManager(t, fake, secret)
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: 37 * time.Second}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "repair git")
	if err != nil || result.Status != core.StatusSucceeded || result.FinalResponse != "task complete" {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
	if fake.startCalls != 1 || fake.stopCalls != 1 || fake.removeCalls != 1 {
		t.Fatalf("container lifecycle = start %d stop %d remove %d", fake.startCalls, fake.stopCalls, fake.removeCalls)
	}
	if fake.created.Name != "aries-openclaw-attempt" || len(fake.created.Config.Cmd) == 0 || len(fake.created.HostConfig.Mounts) != 0 || len(fake.created.HostConfig.Binds) != 0 {
		t.Fatalf("container create = %#v", fake.created)
	}
	fake.mu.Lock()
	var agentCommand string
	for _, exec := range fake.execs {
		joined := strings.Join(exec.Cmd, " ")
		if strings.Contains(joined, " openclaw.mjs agent ") {
			agentCommand = joined
		}
	}
	fake.mu.Unlock()
	if !strings.Contains(agentCommand, " --timeout 37") {
		t.Fatalf("task timeout was not passed to OpenClaw: %q", agentCommand)
	}
	serialized := strings.Join(append(append([]string{}, fake.created.Config.Env...), fake.created.Config.Cmd...), "\n")
	for _, value := range fake.created.Config.Labels {
		serialized += "\n" + value
	}
	if strings.Contains(serialized, "model-secret") {
		t.Fatal("secret entered Docker config")
	}
	files := readArchive(t, fake.archive)
	for path, mode := range map[string]int64{
		"run/aries/openclaw.json": 0o600, "run/aries/model.key": 0o600, "run/aries/gateway.key": 0o600,
		"run/aries/launch": 0o555, "run/aries/ssh/id_ed25519": 0o600, "run/aries/ssh/known_hosts": 0o600,
		"opt/aries/bin/aries-ssh": 0o555,
	} {
		file, ok := files[path]
		if !ok || file.mode != mode || file.uid != 1000 || file.gid != 1000 {
			t.Fatalf("archive %q = %#v", path, file)
		}
	}
	if string(files["run/aries/model.key"].content) != "model-secret" {
		t.Fatal("model key was not staged")
	}
	for _, path := range result.LogPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("artifact %q: %v", path, err)
		}
	}
	configPath := filepath.Join(manager.outputDir, "fix-git", "harness", "openclaw.json")
	configArtifact, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("retained OpenClaw config: %v", err)
	}
	if string(configArtifact) != string(files["run/aries/openclaw.json"].content) || bytes.Contains(configArtifact, []byte("model-secret")) {
		t.Fatal("retained OpenClaw config differs from the staged placeholder-only config")
	}
	if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("retained OpenClaw config mode = %v, %v", info, err)
	}
}

func TestStartFailureRemovesOnlyContainerAndClearsSecret(t *testing.T) {
	fake := newFakeDocker()
	fake.copyToErr = errors.New("copy failed")
	secret := []byte("source-secret")
	manager := newTestManager(t, fake, secret)
	err := manager.Start(context.Background(), core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()})
	if err == nil || fake.removeCalls != 1 || manager.active != nil {
		t.Fatalf("Start = %v, remove=%d active=%v", err, fake.removeCalls, manager.active)
	}
	if !bytes.Equal(secret, make([]byte, len(secret))) {
		t.Fatalf("API lookup buffer was not cleared: %q", secret)
	}
}

func TestArtifactCollectionFailureBelongsToRunNotStop(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fake.containerLogsErr = errors.New("gateway log unavailable")
	result, err := manager.Run(context.Background(), "repair git")
	if err == nil || result.Status != core.StatusFailed || !strings.Contains(err.Error(), "gateway log unavailable") {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if len(result.LogPaths) == 0 {
		t.Fatal("failed Run omitted collected artifact paths")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop was contaminated by artifact collection: %v", err)
	}
	if fake.logsCalls != 1 || fake.removeCalls != 1 {
		t.Fatalf("artifact/lifecycle calls = logs %d remove %d", fake.logsCalls, fake.removeCalls)
	}
}

func TestStopSucceedsWhenForcedRemovalConfirmsAbsence(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fake.stopErr = errors.New("graceful stop failed")
	fake.killErr = errors.New("kill failed")
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop should trust confirmed final absence: %v", err)
	}
	if fake.stopCalls != 1 || fake.killCalls != 1 || fake.removeCalls != 1 {
		t.Fatalf("lifecycle calls = stop %d kill %d remove %d", fake.stopCalls, fake.killCalls, fake.removeCalls)
	}
}

func TestStopFailsUntilContainerAbsenceCanBeConfirmed(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	active := manager.active
	fake.inspectErr = errors.New("daemon unavailable")
	fake.removeErr = errors.New("remove unavailable")
	if err := manager.Stop(context.Background()); err == nil {
		t.Fatal("Stop succeeded without confirming container absence")
	}
	if manager.active != active || active.containerID == "" || len(active.apiKey) == 0 {
		t.Fatal("failed cleanup discarded retry state or secrets before confirmed removal")
	}
	fake.inspectErr = nil
	fake.removeErr = nil
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop: %v", err)
	}
	if manager.active != nil || active.containerID != "" || len(active.apiKey) != 0 {
		t.Fatal("successful retry did not clear lifecycle state and secrets")
	}
}

func TestBuildAgentCommandUsesPinnedDirectExec(t *testing.T) {
	active := &session{safeTaskID: "fix-git", model: testModel()}
	command := buildAgentCommand(active, "repair", 12)
	wantPrefix := []string{launcherPath, "node", "openclaw.mjs", "agent", "--session-key", "agent:main:aries-fix-git", "--message", "repair", "--json", "--timeout", "12"}
	if !equalStrings(command, wantPrefix) {
		t.Fatalf("command = %q", command)
	}
	for _, value := range command {
		if strings.Contains(value, "secret") {
			t.Fatal("secret entered exec argv")
		}
	}
}

type archiveFile struct {
	content []byte
	mode    int64
	uid     int
	gid     int
}

func readArchive(t *testing.T, archive []byte) map[string]archiveFile {
	t.Helper()
	files := make(map[string]archiveFile)
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = archiveFile{content: content, mode: header.Mode, uid: header.Uid, gid: header.Gid}
	}
}

func TestInputValidationAndSecretHelpers(t *testing.T) {
	for _, value := range [][]byte{nil, {}, []byte("bad\nkey"), bytes.Repeat([]byte{'x'}, maxAPIKeyBytes+1)} {
		if err := validateAPIKey(value); err == nil {
			t.Fatalf("validateAPIKey(%q) succeeded", value)
		}
	}
	for _, id := range []string{"", "-bad", "bad/name"} {
		if err := validateRunID(id); err == nil {
			t.Fatalf("validateRunID(%q) succeeded", id)
		}
	}
	if got := safeTaskID(" Fix / Git "); got != "fix---git" {
		t.Fatalf("safeTaskID = %q", got)
	}
	t.Setenv("ARIES_TEST_KEY", "value")
	first, _ := environmentAPIKeyLookup("ARIES_TEST_KEY")
	first[0] = 'X'
	second, _ := environmentAPIKeyLookup("ARIES_TEST_KEY")
	if string(second) != "value" {
		t.Fatalf("environment lookup aliased: %q", second)
	}
}
