package docker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type fakeNotFound struct{ kind string }

func (e fakeNotFound) Error() string { return e.kind + " not found" }
func (fakeNotFound) NotFound()       {}

type observedReader struct {
	reader io.Reader
	done   chan struct{}
	once   sync.Once
}

type cancelErrorWriter struct {
	cancel context.CancelFunc
	err    error
}

func (w cancelErrorWriter) Write([]byte) (int, error) {
	if w.cancel != nil {
		w.cancel()
	}
	return 0, w.err
}

func (r *observedReader) Read(content []byte) (int, error) {
	n, err := r.reader.Read(content)
	if err != nil {
		r.once.Do(func() { close(r.done) })
	}
	return n, err
}

type fakeClient struct {
	dockerClient
	mu sync.Mutex

	networkName    string
	networkOptions client.NetworkCreateOptions
	networkExists  bool
	containerOpts  client.ContainerCreateOptions
	containerID    string
	containerLive  bool
	createErr      error
	execOptions    client.ExecCreateOptions
	execExit       int
	execRunning    bool
	controlExit    int
	controlErr     error
	controlOptions client.ExecCreateOptions
	leaveExecAlive bool
	execCreates    int
	attach         func(net.Conn)
	logs           []byte
	upload         client.CopyToContainerOptions
	uploadBytes    []byte
	uploadErr      error
	download       client.CopyFromContainerResult
	downloadErr    error
	downloadCalls  int
	closeCalls     int
	closeErr       error

	// Docker answers an exec's attach before it starts the process: the first
	// unstartedInspects inspects report an exec it has not started yet, as
	// running (marked, with no process yet) when unstartedRunning is set.
	unstartedInspects int
	unstartedRunning  bool
	failedStart       bool
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return f.closeErr
}

func (f *fakeClient) NetworkCreate(_ context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networkName, f.networkOptions, f.networkExists = name, options, true
	return client.NetworkCreateResult{ID: "network-id"}, nil
}

func (f *fakeClient) NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.networkExists {
		return client.NetworkInspectResult{}, fakeNotFound{"network"}
	}
	return client.NetworkInspectResult{Network: network.Inspect{Network: network.Network{
		ID: "network-id", Name: f.networkName, Labels: f.networkOptions.Labels, Driver: f.networkOptions.Driver, Internal: f.networkOptions.Internal,
		IPAM: network.IPAM{Config: []network.IPAMConfig{{Gateway: netip.MustParseAddr("172.30.0.1")}}},
	}}}, nil
}

func (f *fakeClient) NetworkRemove(context.Context, string, client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networkExists = false
	return client.NetworkRemoveResult{}, nil
}

func (f *fakeClient) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerOpts = options
	if f.createErr != nil {
		return client.ContainerCreateResult{}, f.createErr
	}
	f.containerID = "container-id"
	return client.ContainerCreateResult{ID: f.containerID}, nil
}

func (f *fakeClient) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerLive = true
	return client.ContainerStartResult{}, nil
}

func (f *fakeClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.containerID == "" {
		return client.ContainerInspectResult{}, fakeNotFound{"container"}
	}
	config := f.containerOpts.Config
	if config == nil {
		config = &container.Config{}
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID: f.containerID, State: &container.State{Running: f.containerLive}, Config: config, HostConfig: f.containerOpts.HostConfig,
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{f.networkName: {}}},
	}}, nil
}

type missingSecurityOptClient struct{ *fakeClient }

func (f *missingSecurityOptClient) ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	result, err := f.fakeClient.ContainerInspect(ctx, id, options)
	result.Container.HostConfig = nil
	return result, err
}

func (f *fakeClient) ContainerTop(context.Context, string, client.ContainerTopOptions) (client.ContainerTopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	processes := [][]string{}
	if f.execRunning {
		processes = append(processes, []string{"100", "1", "100"}, []string{"101", "100", "101"})
	}
	return client.ContainerTopResult{Titles: []string{"PID", "PPID", "PGID"}, Processes: processes}, nil
}

func (f *fakeClient) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return io.NopCloser(bytes.NewReader(f.logs)), nil
}

func (f *fakeClient) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerLive = false
	return client.ContainerStopResult{}, nil
}

func (f *fakeClient) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containerID = ""
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeClient) ExecCreate(_ context.Context, _ string, options client.ExecCreateOptions) (client.ExecCreateResult, error) {
	f.mu.Lock()
	f.execCreates++
	if len(options.Cmd) > 2 && options.Cmd[2] == cancelExecShell {
		f.controlOptions = options
		f.mu.Unlock()
		return client.ExecCreateResult{ID: "control-id"}, nil
	}
	f.execOptions = options
	f.execRunning = f.execRunning || f.leaveExecAlive
	f.mu.Unlock()
	return client.ExecCreateResult{ID: "exec-id"}, nil
}

func (f *fakeClient) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	clientConn, daemonConn := net.Pipe()
	f.mu.Lock()
	handler := f.attach
	f.mu.Unlock()
	go func() {
		defer daemonConn.Close()
		if handler != nil {
			handler(daemonConn)
		}
		f.mu.Lock()
		token, exitCode := f.execOptions.Cmd[5], f.execExit
		f.mu.Unlock()
		writeFrame(daemonConn, stdcopy.Stderr, []byte("\x1eARIES_EXEC_EXIT_"+token+"="+strconv.Itoa(exitCode)+"\x1f"))
	}()
	return client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(clientConn, "application/vnd.docker.multiplexed-stream")}, nil
}

func (f *fakeClient) ExecStart(context.Context, string, client.ExecStartOptions) (client.ExecStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.controlErr != nil {
		return client.ExecStartResult{}, f.controlErr
	}
	if f.controlExit == 0 && !f.leaveExecAlive {
		f.execRunning = false
	}
	return client.ExecStartResult{}, nil
}

func (f *fakeClient) ExecInspect(_ context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if execID == "control-id" {
		return client.ExecInspectResult{ID: execID, ContainerID: f.containerID, ExitCode: f.controlExit}, nil
	}
	if f.unstartedInspects > 0 {
		f.unstartedInspects--
		return client.ExecInspectResult{ID: execID, ContainerID: f.containerID, Running: f.unstartedRunning}, nil
	}
	if f.failedStart {
		return client.ExecInspectResult{ID: execID, ContainerID: f.containerID, ExitCode: 126}, nil
	}
	return client.ExecInspectResult{ID: execID, ContainerID: f.containerID, Running: f.execRunning, ExitCode: f.execExit, PID: 100}, nil
}

func (f *fakeClient) CopyToContainer(_ context.Context, _ string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if f.uploadErr != nil {
		return client.CopyToContainerResult{}, f.uploadErr
	}
	content, err := io.ReadAll(options.Content)
	if err != nil {
		return client.CopyToContainerResult{}, err
	}
	f.mu.Lock()
	f.upload, f.uploadBytes = options, content
	f.mu.Unlock()
	return client.CopyToContainerResult{}, nil
}

func (f *fakeClient) CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloadCalls++
	if f.downloadErr != nil {
		return client.CopyFromContainerResult{}, f.downloadErr
	}
	return f.download, nil
}

func startSandbox(t *testing.T, fake *fakeClient) *execution {
	t.Helper()
	return &execution{client: fake, containerID: "container-id", cleanupTimeout: time.Second}
}
func writeFrame(writer io.Writer, stream stdcopy.StdType, payload []byte) {
	header := make([]byte, 8)
	header[0] = byte(stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	_, _ = writer.Write(header)
	_, _ = writer.Write(payload)
}

func TestExecStreamsStdinAndSeparatesOutput(t *testing.T) {
	fake := &fakeClient{execExit: 7}
	fake.attach = func(conn net.Conn) {
		input := make([]byte, len("late\x00stdin"))
		if _, err := io.ReadFull(conn, input); err != nil || string(input) != "late\x00stdin" {
			return
		}
		writeFrame(conn, stdcopy.Stdout, []byte("out"))
		writeFrame(conn, stdcopy.Stderr, []byte("\x1eARIES_EXEC_EXIT_spoof=99\x1ferr"))
	}
	sandbox := startSandbox(t, fake)
	result, err := sandbox.Exec(context.Background(), core.Command{
		Path: "/bin/tool", Args: []string{"arg"}, Dir: "/work", Env: map[string]string{"B": "2", "A": "1"},
		Stdin: []byte("late\x00stdin"),
	})
	if err != nil || result.ExitCode != 7 || result.Stdout != "out" || result.Stderr != "\x1eARIES_EXEC_EXIT_spoof=99\x1ferr" || result.Duration <= 0 {
		t.Fatalf("Exec() = %#v, %v", result, err)
	}
	if len(fake.execOptions.Cmd) != 8 || fake.execOptions.Cmd[2] != execShell || !strings.HasPrefix(fake.execOptions.Cmd[4], execStatePrefix) || !reflect.DeepEqual(fake.execOptions.Cmd[6:], []string{"/bin/tool", "arg"}) || !reflect.DeepEqual(fake.execOptions.Env, []string{"A=1", "B=2"}) {
		t.Fatalf("exec options = %#v", fake.execOptions)
	}
}

func TestExecAcceptsAriesEnvironment(t *testing.T) {
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	if _, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/true", Env: map[string]string{"ARIES_VALID": "value"}}); err != nil {
		t.Fatalf("Exec() rejected valid ARIES_ environment: %v", err)
	}
	if !reflect.DeepEqual(fake.execOptions.Env, []string{"ARIES_VALID=value"}) {
		t.Fatalf("exec environment = %#v", fake.execOptions.Env)
	}
}

func TestExecUserValidationRejectsNamesAndMalformedIDs(t *testing.T) {
	for _, user := range []string{"root", "1000", "1000:", ":1000", "1:2:3", "-1:0", "1.0:2", " 1:2", "1:2 "} {
		t.Run(user, func(t *testing.T) {
			if err := validateCommand(core.Command{Path: "/bin/true", User: user}); err == nil {
				t.Fatalf("validateCommand accepted exec user %q", user)
			}
		})
	}
	for _, user := range []string{"", "0:0", "65532:65532", "0001:0002"} {
		if err := validateCommand(core.Command{Path: "/bin/true", User: user}); err != nil {
			t.Fatalf("validateCommand rejected exec user %q: %v", user, err)
		}
	}
}

func TestExecStreamHonorsConfiguredOutputLimit(t *testing.T) {
	for _, test := range []struct {
		name    string
		limit   int
		wantErr bool
	}{
		{name: "within limit", limit: 4},
		{name: "over limit", limit: 3, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeClient{}
			fake.attach = func(conn net.Conn) {
				writeFrame(conn, stdcopy.Stdout, []byte("four"))
			}
			sandbox := startSandbox(t, fake)
			var stdout bytes.Buffer
			_, err := sandbox.ExecStream(context.Background(), core.Command{
				Path: "/bin/true", OutputLimitBytes: test.limit,
			}, nil, &stdout, io.Discard)
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "output exceeds 3 bytes") {
					t.Fatalf("ExecStream() error = %v", err)
				}
				return
			}
			if err != nil || stdout.String() != "four" {
				t.Fatalf("ExecStream() stdout = %q, error = %v", stdout.String(), err)
			}
		})
	}
}

func TestCommandOutputLimitValidation(t *testing.T) {
	for _, limit := range []int{-1, maxConfiguredOutput + 1} {
		if err := validateCommand(core.Command{Path: "/bin/true", OutputLimitBytes: limit}); err == nil {
			t.Fatalf("validateCommand accepted output limit %d", limit)
		}
	}
}

func TestExecCancellationReturnsTerminationConfirmationFailure(t *testing.T) {
	fake := &fakeClient{execRunning: true, leaveExecAlive: true}
	fake.attach = func(conn net.Conn) { _, _ = io.Copy(io.Discard, conn) }
	sandbox := startSandbox(t, fake)
	sandbox.cleanupTimeout = 60 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := sandbox.ExecStream(ctx, core.Command{Path: "/bin/sleep", Args: []string{"60"}}, nil, io.Discard, io.Discard)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "confirm terminated Docker exec process-group exit") {
		t.Fatalf("ExecStream() error = %v", err)
	}
	fake.mu.Lock()
	execCreates := fake.execCreates
	fake.mu.Unlock()
	if execCreates != 2 {
		t.Fatalf("exec create count = %d, want command plus targeted termination helper", execCreates)
	}
	if fake.controlOptions.User != "0:0" {
		t.Fatalf("termination helper user = %q, want root", fake.controlOptions.User)
	}
}

func TestExecCancellationWinsConcurrentCopyErrorAfterConfirmedTermination(t *testing.T) {
	copyErr := errors.New("attach copy failed")
	fake := &fakeClient{execRunning: true}
	fake.attach = func(conn net.Conn) {
		writeFrame(conn, stdcopy.Stdout, []byte("trigger cancellation"))
	}
	sandbox := startSandbox(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := sandbox.ExecStream(ctx, core.Command{Path: "/bin/sleep", Args: []string{"60"}}, nil,
		cancelErrorWriter{cancel: cancel, err: copyErr}, io.Discard)
	if err != context.Canceled {
		t.Fatalf("ExecStream() error = %v, want exact context cancellation", err)
	}
}

func TestExecCancellationJoinsConcurrentCopyErrorOnlyWithTerminationFailure(t *testing.T) {
	copyErr := errors.New("attach copy failed")
	fake := &fakeClient{execRunning: true, leaveExecAlive: true}
	fake.attach = func(conn net.Conn) {
		writeFrame(conn, stdcopy.Stdout, []byte("trigger cancellation"))
	}
	sandbox := startSandbox(t, fake)
	sandbox.cleanupTimeout = 60 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	_, err := sandbox.ExecStream(ctx, core.Command{Path: "/bin/sleep", Args: []string{"60"}}, nil,
		cancelErrorWriter{cancel: cancel, err: copyErr}, io.Discard)
	if !errors.Is(err, context.Canceled) || errors.Is(err, copyErr) || !strings.Contains(err.Error(), "confirm terminated Docker exec process-group exit") {
		t.Fatalf("ExecStream() error = %v, want cancellation joined only with termination failure", err)
	}
}

func TestExecOrdinaryCopyErrorRetainsCauseAfterTargetedCleanup(t *testing.T) {
	copyErr := errors.New("attach copy failed before cancellation")
	fake := &fakeClient{execRunning: true}
	fake.attach = func(conn net.Conn) {
		writeFrame(conn, stdcopy.Stdout, []byte("trigger copy error"))
	}
	sandbox := startSandbox(t, fake)
	_, err := sandbox.ExecStream(context.Background(), core.Command{Path: "/bin/tool"}, nil,
		cancelErrorWriter{err: copyErr}, io.Discard)
	if err != copyErr {
		t.Fatalf("ExecStream() error = %v, want exact ordinary copy error", err)
	}
}

func TestExecStreamWritesWithoutBufferingResult(t *testing.T) {
	fake := &fakeClient{}
	fake.attach = func(conn net.Conn) {
		input := make([]byte, 4)
		_, _ = io.ReadFull(conn, input)
		writeFrame(conn, stdcopy.Stdout, append([]byte("seen:"), input...))
	}
	sandbox := startSandbox(t, fake)
	var stdout bytes.Buffer
	result, err := sandbox.ExecStream(context.Background(), core.Command{Path: "/bin/cat"}, strings.NewReader("late"), &stdout, io.Discard)
	if err != nil || stdout.String() != "seen:late" || result.Stdout != "" || result.ExitCode != 0 {
		t.Fatalf("ExecStream() = %#v, stdout=%q, error=%v", result, stdout.String(), err)
	}
}

func TestExecStreamReturnsWhileSSHStdinRemainsOpen(t *testing.T) {
	fake := &fakeClient{}
	fake.attach = func(conn net.Conn) {
		writeFrame(conn, stdcopy.Stdout, []byte("complete"))
	}
	sandbox := startSandbox(t, fake)
	pipeReader, pipeWriter := io.Pipe()
	stdinEnded := make(chan struct{})
	stdin := &observedReader{reader: pipeReader, done: stdinEnded}
	type outcome struct {
		result core.CommandResult
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		var stdout bytes.Buffer
		result, err := sandbox.ExecStream(context.Background(), core.Command{Path: "/bin/true"}, stdin, &stdout, io.Discard)
		result.Stdout = stdout.String()
		finished <- outcome{result: result, err: err}
	}()
	select {
	case got := <-finished:
		if got.err != nil || got.result.ExitCode != 0 || got.result.Stdout != "complete" {
			t.Fatalf("ExecStream() = %#v, %v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExecStream waited for SSH stdin EOF after process exit")
	}
	_ = pipeWriter.Close()
	select {
	case <-stdinEnded:
	case <-time.After(time.Second):
		t.Fatal("stdin copier did not finish when the SSH session closed")
	}
}

func TestExecWaitsForAnExecDockerHasNotStarted(t *testing.T) {
	// Taking the first "not running, no PID" inspect for an exit closed the
	// stream after execDrainTimeout, before the command wrote its trailer.
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	fake.mu.Lock()
	fake.execRunning, fake.execExit, fake.unstartedInspects = true, 3, 3
	fake.attach = func(conn net.Conn) {
		time.Sleep(4 * execDrainTimeout)
		writeFrame(conn, stdcopy.Stdout, []byte("done"))
		fake.mu.Lock()
		fake.execRunning = false
		fake.mu.Unlock()
	}
	fake.mu.Unlock()
	result, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/sleep", Args: []string{"1"}})
	if err != nil || result.ExitCode != 3 || result.Stdout != "done" {
		t.Fatalf("Exec() = %#v, %v", result, err)
	}
}

func TestExecDoesNotWaitForAnExecThatFailedToStart(t *testing.T) {
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	fake.mu.Lock()
	fake.failedStart, fake.execExit = true, 126
	fake.mu.Unlock()
	began := time.Now()
	result, err := sandbox.Exec(context.Background(), core.Command{Path: "/missing"})
	if err != nil || result.ExitCode != 126 || time.Since(began) > 5*time.Second {
		t.Fatalf("Exec() = %#v, %v after %s", result, err, time.Since(began))
	}
}

func TestWaitForExecExitGivesUpOnAnExecThatNeverStarts(t *testing.T) {
	sandbox := &execution{client: &fakeClient{unstartedInspects: 1 << 30}}
	err := sandbox.waitForExecExit(context.Background(), "exec-id", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not start within 100ms") {
		t.Fatalf("waitForExecExit() = %v", err)
	}
}

func TestWaitForExecExitGivesUpOnAnExecStuckStarting(t *testing.T) {
	// Docker marks an exec running before it creates the process; the start
	// bound applies to that state too.
	sandbox := &execution{client: &fakeClient{unstartedInspects: 1 << 30, unstartedRunning: true}}
	err := sandbox.waitForExecExit(context.Background(), "exec-id", 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not start within 100ms") {
		t.Fatalf("waitForExecExit() = %v", err)
	}
}

type delayedOutput struct {
	entered chan struct{}
	release chan struct{}
	bytes.Buffer
}

func (w *delayedOutput) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return w.Buffer.Write(p)
}
func TestCancellationJoinsOutputCopyBeforeReturning(t *testing.T) {
	fake := &fakeClient{execRunning: true}
	fake.attach = func(conn net.Conn) { writeFrame(conn, stdcopy.Stdout, []byte("pending output")) }
	output := &delayedOutput{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (&Manager{client: fake}).ExecStream(ctx, "container-id", core.Command{Path: "/bin/tool"}, nil, output, nil)
		done <- err
	}()
	<-output.entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before output copy finished: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(output.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if output.String() != "pending output" {
		t.Fatalf("output=%q", output.String())
	}
}

type observedInspectClient struct {
	*fakeClient
	inspected chan context.Context
	once      sync.Once
}

func (f *observedInspectClient) ExecInspect(ctx context.Context, id string, options client.ExecInspectOptions) (client.ExecInspectResult, error) {
	if _, bounded := ctx.Deadline(); id == "exec-id" && !bounded {
		f.once.Do(func() { f.inspected <- ctx })
	}
	return f.fakeClient.ExecInspect(ctx, id, options)
}
func TestCopyFailureCancelsInspectorAfterFailedTermination(t *testing.T) {
	failure := errors.New("output failed")
	base := &fakeClient{execRunning: true, controlErr: errors.New("termination failed")}
	base.attach = func(conn net.Conn) { writeFrame(conn, stdcopy.Stdout, []byte("output")) }
	fake := &observedInspectClient{fakeClient: base, inspected: make(chan context.Context, 1)}
	_, err := (&Manager{client: fake}).ExecStream(context.Background(), "container-id", core.Command{Path: "/bin/tool"}, nil, cancelErrorWriter{err: failure}, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	inspectorContext := <-fake.inspected
	select {
	case <-inspectorContext.Done():
	case <-time.After(time.Second):
		t.Fatal("exec inspector was left polling after failure")
	}
}
