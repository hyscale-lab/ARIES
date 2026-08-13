package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

var _ runner.LimitedDownloader = (*Sandbox)(nil)

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
	downloadCalls  int
	closeCalls     int
	closeErr       error
}

func (f *fakeClient) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	return f.closeErr
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	closeFailure := errors.New("close failed")
	fake := &fakeClient{closeErr: closeFailure}
	manager := &Manager{client: fake}
	if err := manager.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("error = %v", err)
	}
	if err := manager.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("error = %v", err)
	}
	if fake.closeCalls != 1 {
		t.Fatalf("close calls = %d", fake.closeCalls)
	}
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
		Name: f.networkName, Labels: f.networkOptions.Labels,
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
		ID: f.containerID, State: &container.State{Running: f.containerLive}, Config: config,
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{f.networkName: {}}},
	}}, nil
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
	return f.download, nil
}

func testEnvironment() core.Environment {
	return core.Environment{
		Image: "example.invalid/task:fixture@sha256:" + strings.Repeat("a", 64), Workdir: "/work",
		CPU: 1.5, MemoryMB: 64, StorageMB: 32, GPUs: 1,
		Env: map[string]string{"ZED": "last", "ALPHA": "first"},
	}
}

func testRequest() core.SandboxRequest {
	return core.SandboxRequest{RunID: "run-1", TaskID: "task-1", Environment: testEnvironment()}
}

func testManager(t *testing.T, fake *fakeClient) *Manager {
	t.Helper()
	return &Manager{
		client: fake, outputDir: t.TempDir(), cleanupTimeout: time.Second,
		logger: logrus.New(),
		newID:  func() (string, error) { return "fixedid", nil },
	}
}

func startSandbox(t *testing.T, fake *fakeClient) *Sandbox {
	t.Helper()
	_, sandbox := startManagedSandbox(t, fake)
	return sandbox
}

func startManagedSandbox(t *testing.T, fake *fakeClient) (*Manager, *Sandbox) {
	t.Helper()
	manager := testManager(t, fake)
	live, err := manager.Start(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return manager, live.(*Sandbox)
}

func TestStartUsesTypedOptionsAndStopIsIdempotent(t *testing.T) {
	t.Setenv("TZ", "")
	logs := multiplexed("sandbox stdout", "sandbox stderr")
	fake := &fakeClient{logs: logs}
	manager, sandbox := startManagedSandbox(t, fake)
	if sandbox.artifactDir != filepath.Join(manager.outputDir, "task-1", "sandbox") {
		t.Fatalf("artifact directory = %q", sandbox.artifactDir)
	}
	options := fake.containerOpts
	if options.Name != "aries-task-fixedid" || options.Config.WorkingDir != "/work" {
		t.Fatalf("container options = %#v", options)
	}
	if !reflect.DeepEqual(options.Config.Env, []string{"ALPHA=first", "DEBIAN_FRONTEND=noninteractive", "TZ=UTC", "ZED=last"}) {
		t.Fatalf("environment = %#v", options.Config.Env)
	}
	resources := options.HostConfig.Resources
	if resources.NanoCPUs != 1_500_000_000 || resources.Memory != 64<<20 || options.HostConfig.StorageOpt["size"] != "32m" {
		t.Fatalf("resources = %#v", options.HostConfig)
	}
	if len(resources.DeviceRequests) != 1 || resources.DeviceRequests[0].Count != 1 {
		t.Fatalf("GPU request = %#v", resources.DeviceRequests)
	}
	if !fake.networkOptions.Internal || fake.networkOptions.Labels["aries.kind"] != "task-network" || options.Config.Labels["aries.kind"] != "task-container" || options.Config.Labels["aries.component"] != "sandbox" {
		t.Fatalf("network/container labels = %#v / %#v", fake.networkOptions, options.Config.Labels)
	}
	if gateway, err := sandbox.NetworkGateway(context.Background()); err != nil || gateway != "172.30.0.1" {
		t.Fatalf("NetworkGateway() = %q, %v", gateway, err)
	}
	if err := manager.Stop(context.Background(), sandbox); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := manager.Stop(context.Background(), sandbox); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
	stdout, _ := os.ReadFile(filepath.Join(sandbox.artifactDir, "container.stdout.log"))
	stderr, _ := os.ReadFile(filepath.Join(sandbox.artifactDir, "container.stderr.log"))
	if string(stdout) != "sandbox stdout" || string(stderr) != "sandbox stderr" {
		t.Fatalf("logs = %q / %q", stdout, stderr)
	}
	if fake.containerID != "" || fake.networkExists {
		t.Fatal("Stop left fake resources behind")
	}
}

func TestContainerOptionsEnablesNoNewPrivilegesForConfiguredExecUser(t *testing.T) {
	request := testRequest()
	sandbox := &Sandbox{containerName: "task", networkName: "network"}
	if got := containerOptions(request, sandbox, nil).HostConfig.SecurityOpt; got != nil {
		t.Fatalf("default security options = %v", got)
	}
	request.Environment.ExecUser = "65532:65532"
	got := containerOptions(request, sandbox, nil).HostConfig.SecurityOpt
	if !reflect.DeepEqual(got, []string{"no-new-privileges=true"}) {
		t.Fatalf("non-root security options = %v", got)
	}
}

func TestTaskEnvironmentUsesHostTimezoneAndAriesPrecedence(t *testing.T) {
	t.Setenv("TZ", "America/Los_Angeles")
	request := testRequest()
	request.Environment.Env = map[string]string{"TZ": "task-zone", "DEBIAN_FRONTEND": "dialog", "KEEP": "exact"}
	fake := &fakeClient{}
	manager := testManager(t, fake)
	if _, err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	want := []string{"DEBIAN_FRONTEND=noninteractive", "KEEP=exact", "TZ=America/Los_Angeles"}
	if !reflect.DeepEqual(fake.containerOpts.Config.Env, want) {
		t.Fatalf("environment = %#v, want %#v", fake.containerOpts.Config.Env, want)
	}
}

func TestValidateEnvironmentRejectsResourceConversionOverflow(t *testing.T) {
	environment := testEnvironment()
	environment.CPU = math.Exp2(63) / 1e9
	if err := validateEnvironment(environment); err == nil {
		t.Fatal("accepted overflowing CPU")
	}
	environment = testEnvironment()
	environment.MemoryMB = int(math.MaxInt64>>20) + 1
	if err := validateEnvironment(environment); err == nil {
		t.Fatal("accepted overflowing memory")
	}
}

func TestRootAcceptedOnlyAsWorkdir(t *testing.T) {
	environment := testEnvironment()
	environment.Workdir = "/"
	if err := validateEnvironment(environment); err != nil {
		t.Fatalf("validateEnvironment(root workdir): %v", err)
	}

	request := testRequest()
	request.Environment.Workdir = "/"
	fake := &fakeClient{}
	manager := testManager(t, fake)
	sandbox, err := manager.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background(), sandbox) })
	if fake.containerOpts.Config.WorkingDir != "/" {
		t.Fatalf("Docker WorkingDir = %q, want root", fake.containerOpts.Config.WorkingDir)
	}

	if _, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/true", Dir: "/"}); err != nil {
		t.Fatalf("Exec(root workdir): %v", err)
	}
	if fake.execOptions.WorkingDir != "/" {
		t.Fatalf("Docker exec WorkingDir = %q, want root", fake.execOptions.WorkingDir)
	}
	for _, command := range []core.Command{
		{Path: "/bin/true", Dir: "relative"},
		{Path: "/bin/true", Dir: "/unclean/.."},
		{Path: "/bin/true", Dir: "/bad\x00dir"},
	} {
		if err := validateCommand(command); err == nil {
			t.Fatalf("validateCommand(%#v) unexpectedly succeeded", command)
		}
	}
	if err := validateCommand(core.Command{Path: "/", Dir: "/work"}); err == nil {
		t.Fatal("validateCommand accepted root executable path")
	}

	for _, value := range []string{"", "relative", "/unclean/..", "/bad\x00dir"} {
		environment := testEnvironment()
		environment.Workdir = value
		if err := validateEnvironment(environment); err == nil {
			t.Fatalf("validateEnvironment accepted workdir %q", value)
		}
	}
}

func TestTransferPathsStillRejectRoot(t *testing.T) {
	sandbox := &Sandbox{client: &fakeClient{}, outputDir: t.TempDir()}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Upload(context.Background(), source, "/"); err == nil {
		t.Fatal("Upload accepted root destination")
	}
	if err := sandbox.Download(context.Background(), "/", filepath.Join(sandbox.outputDir, "result")); err == nil {
		t.Fatal("Download accepted root source")
	}
}

func TestManagerStopRejectsNilAndForeignSandbox(t *testing.T) {
	owner, sandbox := startManagedSandbox(t, &fakeClient{})
	t.Cleanup(func() { _ = owner.Stop(context.Background(), sandbox) })

	if err := owner.Stop(context.Background(), nil); err == nil {
		t.Fatal("Stop accepted a nil sandbox")
	}
	other := testManager(t, &fakeClient{})
	if err := other.Stop(context.Background(), sandbox); err == nil || !strings.Contains(err.Error(), "another manager") {
		t.Fatalf("Stop foreign sandbox error = %v", err)
	}
}

func TestStartRollsBackNetworkOnContainerFailure(t *testing.T) {
	want := errors.New("create failed")
	fake := &fakeClient{createErr: want}
	_, err := testManager(t, fake).Start(context.Background(), testRequest())
	if !errors.Is(err, want) {
		t.Fatalf("Start() error = %v", err)
	}
	if fake.networkExists {
		t.Fatal("failed Start left network behind")
	}
}

func TestCommandOutputLimitIsNotSerialized(t *testing.T) {
	encoded, err := json.Marshal(core.Command{
		Path: "/bin/true", OutputLimitBytes: 123456789,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "123456789") || strings.Contains(string(encoded), "OutputLimit") {
		t.Fatalf("internal output limit leaked into JSON: %s", encoded)
	}
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
	defer sandbox.stop(context.Background())
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
	defer sandbox.stop(context.Background())
	if _, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/true", Env: map[string]string{"ARIES_VALID": "value"}}); err != nil {
		t.Fatalf("Exec() rejected valid ARIES_ environment: %v", err)
	}
	if !reflect.DeepEqual(fake.execOptions.Env, []string{"ARIES_VALID=value"}) {
		t.Fatalf("exec environment = %#v", fake.execOptions.Env)
	}
}

func TestExecUsesConfiguredUserAndAllowsExplicitOverride(t *testing.T) {
	fake := &fakeClient{}
	request := testRequest()
	request.Environment.ExecUser = "65532:65532"
	manager := testManager(t, fake)
	live, err := manager.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*Sandbox)
	defer sandbox.stop(context.Background())

	if _, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/true"}); err != nil {
		t.Fatal(err)
	}
	if fake.execOptions.User != "65532:65532" {
		t.Fatalf("default Docker exec user = %q", fake.execOptions.User)
	}
	if _, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/true", User: "0:0"}); err != nil {
		t.Fatal(err)
	}
	if fake.execOptions.User != "0:0" {
		t.Fatalf("explicit Docker exec user = %q", fake.execOptions.User)
	}
}

func TestExecUserValidationRejectsNamesAndMalformedIDs(t *testing.T) {
	for _, user := range []string{"root", "1000", "1000:", ":1000", "1:2:3", "-1:0", "1.0:2", " 1:2", "1:2 "} {
		t.Run(user, func(t *testing.T) {
			environment := testEnvironment()
			environment.ExecUser = user
			if err := validateEnvironment(environment); err == nil {
				t.Fatalf("validateEnvironment accepted exec user %q", user)
			}
			if err := validateCommand(core.Command{Path: "/bin/true", User: user}); err == nil {
				t.Fatalf("validateCommand accepted exec user %q", user)
			}
		})
	}
	for _, user := range []string{"", "0:0", "65532:65532", "0001:0002"} {
		environment := testEnvironment()
		environment.ExecUser = user
		if err := validateEnvironment(environment); err != nil {
			t.Fatalf("validateEnvironment rejected exec user %q: %v", user, err)
		}
		if err := validateCommand(core.Command{Path: "/bin/true", User: user}); err != nil {
			t.Fatalf("validateCommand rejected exec user %q: %v", user, err)
		}
	}
}

func TestInternalExecUsersAreNotSerialized(t *testing.T) {
	encoded, err := json.Marshal(struct {
		Environment core.Environment `json:"environment"`
		Command     core.Command     `json:"command"`
	}{
		Environment: core.Environment{ExecUser: "65532:65532"},
		Command:     core.Command{User: "0:0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "65532") || strings.Contains(string(encoded), "0:0") || strings.Contains(string(encoded), "ExecUser") || strings.Contains(string(encoded), "User") {
		t.Fatalf("internal exec users leaked into JSON: %s", encoded)
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
			defer sandbox.stop(context.Background())
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
	defer sandbox.stop(context.Background())
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
	defer sandbox.stop(context.Background())
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
	defer sandbox.stop(context.Background())
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
	defer sandbox.stop(context.Background())
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
	defer sandbox.stop(context.Background())
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
	defer sandbox.stop(context.Background())
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

func TestUploadAndDownloadUseDockerArchives(t *testing.T) {
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	source := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(source, []byte("upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.Upload(context.Background(), source, "/work/destination.bin"); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	header, content := readArchive(t, fake.uploadBytes)
	if fake.upload.DestinationPath != "/work" || header.Name != "destination.bin" || string(content) != "upload" {
		t.Fatalf("upload archive = path %q, header %#v, content %q", fake.upload.DestinationPath, header, content)
	}

	downloadArchive := archiveFile(t, "source.bin", []byte("download"))
	fake.download = client.CopyFromContainerResult{
		Content: io.NopCloser(bytes.NewReader(downloadArchive)),
		Stat:    container.PathStat{Name: "source.bin", Size: 8, Mode: 0o600},
	}
	destination := filepath.Join(sandbox.outputDir, "evaluation", "result.bin")
	if err := sandbox.Download(context.Background(), "/work/source.bin", destination); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	downloaded, err := os.ReadFile(destination)
	if err != nil || string(downloaded) != "download" {
		t.Fatalf("download = %q, %v", downloaded, err)
	}
	if err := sandbox.Download(context.Background(), "/work/source.bin", filepath.Join(sandbox.outputDir, "..", "escape")); err == nil {
		t.Fatal("Download accepted destination outside output root")
	}
}

func TestDownloadLimitRejectsInvalidLimitBeforeDocker(t *testing.T) {
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	destination := filepath.Join(sandbox.outputDir, "evaluation", "result.bin")
	if err := sandbox.DownloadLimit(context.Background(), "/work/source.bin", destination, -1); err == nil {
		t.Fatal("DownloadLimit accepted a negative limit")
	}
	if fake.downloadCalls != 0 {
		t.Fatalf("Docker download calls = %d, want zero", fake.downloadCalls)
	}
	if _, err := os.Stat(filepath.Dir(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination directory created before rejecting limit: %v", err)
	}
}

func TestDownloadLimitRejectsOversizedStatBeforeReadingArchive(t *testing.T) {
	content := &trackingReadCloser{}
	fake := &fakeClient{download: client.CopyFromContainerResult{
		Content: content,
		Stat:    container.PathStat{Name: "source.bin", Size: 9, Mode: 0o600},
	}}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	destination := filepath.Join(sandbox.outputDir, "evaluation", "result.bin")
	if err := sandbox.DownloadLimit(context.Background(), "/work/source.bin", destination, 8); err == nil {
		t.Fatal("DownloadLimit accepted oversized Docker stat")
	}
	if content.reads != 0 || !content.closed {
		t.Fatalf("archive reads = %d, closed = %t", content.reads, content.closed)
	}
	if _, err := os.Stat(filepath.Dir(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination directory created for oversized stat: %v", err)
	}
}

func TestDownloadLimitRejectsOversizedArchiveHeaderBeforeWriting(t *testing.T) {
	archive := archiveFile(t, "source.bin", []byte("123456789"))
	fake := &fakeClient{download: client.CopyFromContainerResult{
		Content: io.NopCloser(bytes.NewReader(archive)),
		Stat:    container.PathStat{Name: "source.bin", Size: 8, Mode: 0o600},
	}}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	destination := filepath.Join(sandbox.outputDir, "evaluation", "result.bin")
	if err := sandbox.DownloadLimit(context.Background(), "/work/source.bin", destination, 8); err == nil {
		t.Fatal("DownloadLimit accepted oversized archive header")
	}
	if _, err := os.Stat(filepath.Dir(destination)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination directory created for oversized header: %v", err)
	}
}

func TestDownloadLimitPublishesFileWithinLimit(t *testing.T) {
	archive := archiveFile(t, "source.bin", []byte("download"))
	fake := &fakeClient{download: client.CopyFromContainerResult{
		Content: io.NopCloser(bytes.NewReader(archive)),
		Stat:    container.PathStat{Name: "source.bin", Size: 8, Mode: 0o600},
	}}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	destination := filepath.Join(sandbox.outputDir, "evaluation", "result.bin")
	if err := sandbox.DownloadLimit(context.Background(), "/work/source.bin", destination, 8); err != nil {
		t.Fatalf("DownloadLimit() error = %v", err)
	}
	content, err := os.ReadFile(destination)
	if err != nil || string(content) != "download" {
		t.Fatalf("download = %q, %v", content, err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("download mode = %v", info.Mode())
	}
}

type trackingReadCloser struct {
	reads  int
	closed bool
}

func (r *trackingReadCloser) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("unexpected archive read")
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestUploadUnblocksArchiveProducerWhenDockerRejectsStream(t *testing.T) {
	uploadFailure := errors.New("copy rejected")
	fake := &fakeClient{uploadErr: uploadFailure}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	source := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(source, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- sandbox.Upload(context.Background(), source, "/work/destination.bin") }()
	select {
	case err := <-finished:
		if !errors.Is(err, uploadFailure) {
			t.Fatalf("Upload() error = %v, want %v", err, uploadFailure)
		}
	case <-time.After(time.Second):
		t.Fatal("Upload blocked after Docker stopped reading its archive stream")
	}
}

func TestValidationRejectsUnsafeInputsBeforeDocker(t *testing.T) {
	fake := &fakeClient{}
	manager := testManager(t, fake)
	request := testRequest()
	request.RunID = "../escape"
	if _, err := manager.Start(context.Background(), request); err == nil {
		t.Fatal("Start accepted unsafe identity")
	}
	request = testRequest()
	request.Environment.Image = "busybox"
	if _, err := manager.Start(context.Background(), request); err == nil {
		t.Fatal("Start accepted image without an explicit tag or digest")
	}
}

func TestValidateEnvironmentAcceptsExplicitTaskTag(t *testing.T) {
	environment := testRequest().Environment
	environment.Image = "registry.example:5000/org/task:20251031"
	if err := validateEnvironment(environment); err != nil {
		t.Fatal(err)
	}
}

func multiplexed(stdout, stderr string) []byte {
	var result bytes.Buffer
	writeFrame(&result, stdcopy.Stdout, []byte(stdout))
	writeFrame(&result, stdcopy.Stderr, []byte(stderr))
	return result.Bytes()
}

func writeFrame(writer io.Writer, stream stdcopy.StdType, payload []byte) {
	header := make([]byte, 8)
	header[0] = byte(stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
	_, _ = writer.Write(header)
	_, _ = writer.Write(payload)
}

func archiveFile(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var result bytes.Buffer
	writer := tar.NewWriter(&result)
	if err := writer.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o600}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func readArchive(t *testing.T, content []byte) (*tar.Header, []byte) {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(content))
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return header, payload
}
