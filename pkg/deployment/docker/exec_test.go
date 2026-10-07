package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeDocker struct {
	dockerClient
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
	stderrOnly       bool
	nextExec         int
	startCalls       int
	logsCalls        int
	stopCalls        int
	killCalls        int
	removeCalls      int
	createCalls      int
	closeCalls       int
	closeErr         error

	// Docker answers an exec's attach before it starts the process: the first
	// unstartedInspects inspects report an exec it has not started yet, as
	// running (marked, with no process yet) when unstartedRunning is set.
	unstartedInspects int
	unstartedRunning  bool
	execDelay         time.Duration
}

func (fake *fakeDocker) Close() error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.closeCalls++
	return fake.closeErr
}

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
		ID: "openclaw-id", Name: "/" + options.Name, Config: options.Config, HostConfig: options.HostConfig,
		State:           &container.State{},
		NetworkSettings: &container.NetworkSettings{Ports: network.PortMap{}},
	}
	for port, bindings := range options.HostConfig.PortBindings {
		copied := append([]network.PortBinding(nil), bindings...)
		for index := range copied {
			if copied[index].HostPort == "" {
				copied[index].HostPort = "38089"
			}
			if !copied[index].HostIP.IsValid() {
				copied[index].HostIP = netip.MustParseAddr("127.0.0.1")
			}
		}
		fake.container.NetworkSettings.Ports[port] = copied
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
		time.Sleep(fake.execDelay)
		exitCode := 0
		if len(options.Cmd) > 6 && options.Cmd[6] == "/launcher" {
			if !fake.stderrOnly {
				_ = writeMux(engineSide, stdcopy.Stdout, []byte(`{"status":"ok","result":{"payloads":[{"text":"task complete"}]}}`))
			}
			_ = writeMux(engineSide, stdcopy.Stderr, []byte("agent diagnostic\n"))
		}
		if len(options.Cmd) > 4 {
			trailer := fmt.Sprintf("\x1eARIES_EXEC_EXIT_%s=%d\x1f", options.Cmd[5], exitCode)
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
	if fake.unstartedInspects > 0 {
		fake.unstartedInspects--
		return client.ExecInspectResult{ID: execID, ContainerID: fake.container.ID, Running: fake.unstartedRunning}, nil
	}
	pid := 0
	if _, started := fake.execs[execID]; started {
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

func TestExecAttachedWaitsForAnExecDockerHasNotStarted(t *testing.T) {
	// Taking the first "not running, no PID" inspect for an exit closed the
	// stream after the drain, before the command wrote its trailer.
	fake := newFakeDocker()
	fake.container.ID = "container"
	fake.container.State = &container.State{Running: true}
	fake.unstartedInspects = 3
	fake.execDelay = 800 * time.Millisecond
	manager := &Manager{client: fake}
	result, err := manager.Exec(context.Background(), "container", core.Command{Path: "true", Dir: "/"})
	if err != nil || result.ExitCode != 0 || result.Stdout != "" {
		t.Fatalf("execAttached() = %#v, %v", result, err)
	}
}

func TestExecCompletesWhenDockerKeepsAttachOpen(t *testing.T) {
	fake := newFakeDocker()
	fake.container.ID = "container"
	fake.container.State = &container.State{Running: true}
	fake.keepAttachOpen = true
	manager := &Manager{client: fake}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := manager.Exec(ctx, "container", core.Command{Path: "/launcher"})
	if err != nil || result.ExitCode != 0 || result.Stderr != "agent diagnostic\n" || !strings.Contains(result.Stdout, "task complete") {
		t.Fatalf("result=%+v, err=%v", result, err)
	}
}
