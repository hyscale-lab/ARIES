package apptainer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// fakeTaskNamespace stands in for a started task namespace.
type fakeTaskNamespace struct {
	name    string
	request taskNamespaceRequest
	invalid error
	stopErr error
	stops   int
}

func (n *fakeTaskNamespace) Name() string                 { return n.name }
func (n *fakeTaskNamespace) Validate() error              { return n.invalid }
func (n *fakeTaskNamespace) Stop(_ context.Context) error { n.stops++; return n.stopErr }

func TestRunEnvironmentGivesEachTaskANamespaceAndItsServicesItsLoopback(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{})
	environment := manager.NewRunEnvironment("run-1", "http://127.0.0.1:30000/v1")
	network := "net:[4026532001]"
	environment.current = func() (string, error) { return network, nil }
	var started []*fakeTaskNamespace
	environment.startNamespace = func(_ context.Context, request taskNamespaceRequest) (taskNamespaceHandle, error) {
		namespace := &fakeTaskNamespace{name: "ns-" + strconv.Itoa(len(started)+1), request: request}
		started = append(started, namespace)
		return namespace, nil
	}
	ctx := context.Background()
	placement, err := environment.Start(ctx)
	if err != nil || placement.AttachmentID != "run-namespace:net:[4026532001]" {
		t.Fatalf("Start = %+v, %v", placement, err)
	}
	request := deployment.TaskEnvironmentRequest{RuntimeName: "aries-task-1"}
	request.RunID, request.TaskID = "run-1", "task-1"
	request.Environment.Services = core.TaskServices{SearchPort: 8000, MCP: []core.TaskMCPService{{Name: "toolathlon", Port: 10086, Path: "/sse", Transport: "sse"}}}
	first := environment.NewTaskEnvironment()
	connectivity, err := first.Start(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if connectivity.SearchURL != "http://127.0.0.1:8000" || len(connectivity.MCPServers) != 1 || connectivity.MCPServers[0].URL != "http://127.0.0.1:10086/sse" || connectivity.Placement.AttachmentID != "task-namespace:ns-1" {
		t.Fatalf("connectivity = %+v", connectivity)
	}
	// The run's model is on Runner's loopback; the harness reaches it through
	// the task's gateway.
	if connectivity.ModelBaseURL != "http://10.0.3.2:30000/v1" {
		t.Fatalf("harness model URL = %q", connectivity.ModelBaseURL)
	}
	request.RuntimeName, request.TaskID = "aries-task-2", "task-2"
	second := environment.NewTaskEnvironment()
	if connectivity, err := second.Start(ctx, request); err != nil || connectivity.Placement.AttachmentID != "task-namespace:ns-2" {
		t.Fatalf("second task = %+v, %v", connectivity, err)
	}
	if len(started) != 2 {
		t.Fatalf("%d task namespaces started, want 2", len(started))
	}
	if started[0].request.loopback != "127.1.0.1" || started[1].request.loopback != "127.1.0.2" ||
		started[0].request.stateDir != manager.options.StateDir || started[1].request.runtime != "aries-task-2" ||
		started[0].request.labels["aries.task"] != "task-1" || started[1].request.labels["aries.run"] != "run-1" {
		t.Fatalf("namespaces started with %+v and %+v", started[0].request, started[1].request)
	}
	if err := first.Validate(ctx); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	started[0].invalid = errors.New("task namespace ns-1 lost its holder")
	if err := first.Validate(ctx); err == nil {
		t.Fatal("Validate accepted a namespace that lost its holder")
	}
	started[0].invalid = nil
	network = "net:[4026532999]"
	if err := first.Validate(ctx); err == nil {
		t.Fatal("Validate accepted a process that left its namespace")
	}
	// A namespace that still holds a runtime's processes stays, and Stop can be
	// repeated once they are gone.
	started[1].stopErr = errors.New("task namespace ns-2 still holds processes [5000]")
	if err := second.Stop(ctx); err == nil {
		t.Fatal("Stop ignored a namespace that could not end")
	}
	started[1].stopErr = nil
	for _, task := range []deployment.TaskEnvironment{first, second, first} {
		if err := task.Stop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if started[0].stops != 2 || started[1].stops != 2 {
		t.Fatalf("stops = %d, %d", started[0].stops, started[1].stops)
	}
	if err := environment.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	other := manager.NewRunEnvironment("run-2", "")
	other.current = func() (string, error) { return "", errors.New("not in a run namespace") }
	if _, err := other.Start(ctx); err == nil {
		t.Fatal("Start succeeded outside a run namespace")
	}
}

func TestHarnessModelURLReplacesOnlyWhatTheGatewayLeadsTo(t *testing.T) {
	for base, want := range map[string]string{
		"http://127.0.0.1:30000/v1":    "http://10.0.3.2:30000/v1",
		"http://localhost:30000/v1":    "http://10.0.3.2:30000/v1",
		"http://localhost.:30000/v1":   "http://10.0.3.2:30000/v1",
		"http://0.0.0.0:30000/v1":      "http://10.0.3.2:30000/v1",
		"http://127.0.0.1/v1":          "http://10.0.3.2/v1",
		"http://127.0.0.2:30000/v1":    "",
		"http://[::1]:30000/v1":        "",
		"https://127.0.0.1:30000/v1":   "",
		"http://10.0.2.100:30000/v1":   "",
		"http://10.150.7.122:30000/v1": "",
		"https://api.deepseek.com":     "",
		"":                             "",
	} {
		if got := harnessModelURL(base); got != want {
			t.Errorf("harnessModelURL(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestCheckModelAddressRefusesTheRunsOwnSubnets(t *testing.T) {
	for base, want := range map[string]string{
		"http://10.0.2.15:30000/v1":         "10.0.2.0/24 is the subnet slirp4netns gives the run's",
		"http://10.0.2.100:30000/v1":        "10.0.2.0/24 is the subnet slirp4netns gives the run's",
		"http://10.0.3.15:30000/v1":         "10.0.3.0/24 is the subnet slirp4netns gives each task's",
		"http://10.0.3.2:30000/v1":          "10.0.3.0/24",
		"http://[::ffff:10.0.3.9]:30000/v1": "10.0.3.0/24",
		"http://10.0.4.1:30000/v1":          "",
		"http://10.150.7.122:30000/v1":      "",
		"http://127.0.0.1:30000/v1":         "",
		"http://model.cluster:30000/v1":     "",
		"https://api.deepseek.com":          "",
		"":                                  "",
	} {
		err := CheckModelAddress(base, false)
		if want == "" && err != nil || want != "" && (err == nil || !strings.Contains(err.Error(), want)) {
			t.Errorf("CheckModelAddress(%q) = %v, want %q", base, err, want)
		}
	}
}

func TestCheckModelAddressKeepsAManagedServerWhereTheGatewayLeads(t *testing.T) {
	for base, refused := range map[string]bool{
		"http://127.0.0.1:30000/v1":    false,
		"http://localhost:30000/v1":    false,
		"http://localhost.:30000/v1":   false,
		"http://0.0.0.0:30000/v1":      false,
		"http://127.0.0.2:30000/v1":    true,
		"http://[::1]:30000/v1":        true,
		"https://127.0.0.1:30000/v1":   true,
		"https://localhost:30000/v1":   true,
		"https://localhost.:30000/v1":  true,
		"http://10.150.7.122:30000/v1": false,
		"http://10.0.2.100:30000/v1":   false,
		"https://10.0.2.100:30000/v1":  false,
	} {
		err := CheckModelAddress(base, true)
		if refused != (err != nil) || refused && !strings.Contains(err.Error(), "cannot reach a managed model server") {
			t.Errorf("CheckModelAddress(%q, managed) = %v, want refused %v", base, err, refused)
		}
	}
}

func TestATaskNamespaceThatCouldNotBeEndedIsKeptForStop(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{})
	environment := manager.NewRunEnvironment("run-1", "")
	environment.current = func() (string, error) { return "net:[4026532001]", nil }
	// The start failed, and so did removing what it had made.
	leftover := &fakeTaskNamespace{name: "ns-1", stopErr: errors.New("remove task namespace ns-1: permission denied")}
	environment.startNamespace = func(context.Context, taskNamespaceRequest) (taskNamespaceHandle, error) {
		return leftover, errors.New("slirp4netns did not configure the task namespace; remove task namespace ns-1: permission denied")
	}
	ctx := context.Background()
	if _, err := environment.Start(ctx); err != nil {
		t.Fatal(err)
	}
	request := deployment.TaskEnvironmentRequest{RuntimeName: "aries-task-1"}
	request.RunID = "run-1"
	task := environment.NewTaskEnvironment()
	if _, err := task.Start(ctx, request); err == nil {
		t.Fatal("Start succeeded")
	}
	// Stop fails while the namespace cannot be ended, and ends it once it can.
	if err := task.Stop(ctx); err == nil {
		t.Fatal("Stop dropped a namespace it could not end")
	}
	leftover.stopErr = nil
	if err := task.Stop(ctx); err != nil {
		t.Fatalf("Stop once the namespace can end: %v", err)
	}
	if leftover.stops != 2 {
		t.Fatalf("stops = %d, want 2", leftover.stops)
	}
}

func TestATaskWhoseNamespaceDidNotStartHasNothingToStop(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{})
	environment := manager.NewRunEnvironment("run-1", "")
	environment.current = func() (string, error) { return "net:[4026532001]", nil }
	environment.startNamespace = func(context.Context, taskNamespaceRequest) (taskNamespaceHandle, error) {
		return nil, errors.New("slirp4netns did not configure the task namespace")
	}
	ctx := context.Background()
	if _, err := environment.Start(ctx); err != nil {
		t.Fatal(err)
	}
	request := deployment.TaskEnvironmentRequest{RuntimeName: "aries-task-1"}
	request.RunID = "run-1"
	task := environment.NewTaskEnvironment()
	if _, err := task.Start(ctx, request); err == nil || !strings.Contains(err.Error(), "did not configure") {
		t.Fatalf("Start = %v", err)
	}
	if err := task.Validate(ctx); err == nil {
		t.Fatal("Validate accepted a task without a namespace")
	}
	if err := task.Stop(ctx); err != nil {
		t.Fatalf("Stop after a failed Start: %v", err)
	}
}

func TestCurrentRunNamespaceComparesWithTheParent(t *testing.T) {
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Skip("no /proc network namespace link")
	}
	t.Setenv(runNamespaceEnv, current)
	if _, err := currentRunNamespace(); err == nil {
		t.Fatal("accepted the parent's own namespace")
	}
	t.Setenv(runNamespaceEnv, "net:[1]")
	if got, err := currentRunNamespace(); err != nil || got != current {
		t.Fatalf("currentRunNamespace = %q, %v", got, err)
	}
	if inside, err := WaitForRunNamespace(); !inside || err == nil {
		t.Fatalf("a value without a release descriptor = %v, %v", inside, err)
	}
	if got := withoutRunNamespace([]string{"A=1", runNamespaceEnv + "=x", "B=2"}); !slices.Equal(got, []string{"A=1", "B=2"}) {
		t.Fatalf("withoutRunNamespace = %q", got)
	}
}

func TestWaitForRunNamespaceIgnoresOrdinaryProcesses(t *testing.T) {
	t.Setenv(runNamespaceEnv, "")
	os.Unsetenv(runNamespaceEnv)
	if inside, err := WaitForRunNamespace(); inside || err != nil {
		t.Fatalf("WaitForRunNamespace = %v, %v", inside, err)
	}
}

// fakeProgram records process runtime launches. A leader in alive ends at
// any signal; a group in lingering keeps a member until SIGKILL.
type fakeProgram struct {
	mu        sync.Mutex
	starts    [][]string
	dirs      []string
	envs      [][]string
	alive     map[int]bool
	lingering map[int]bool
	inspect   error
	signals   []syscall.Signal
}

func (f *fakeProgram) start(path string, args []string, dir string, environment []string, _, _ *os.File) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, append([]string{path}, args...))
	f.dirs = append(f.dirs, dir)
	f.envs = append(f.envs, environment)
	pid := 7000 + len(f.starts)
	f.alive[pid] = true
	return pid, nil
}

func (f *fakeProgram) processStart(pid int) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspect != nil {
		return 0, false, f.inspect
	}
	return uint64(pid), f.alive[pid], nil
}

func (f *fakeProgram) signal(pgid int, signal syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if signal == 0 {
		if f.alive[pgid] || f.lingering[pgid] {
			return nil
		}
		return syscall.ESRCH
	}
	f.signals = append(f.signals, signal)
	delete(f.alive, pgid)
	if signal == syscall.SIGKILL {
		delete(f.lingering, pgid)
	}
	return nil
}

func newTestProcessRuntime(t *testing.T) (*ProcessRuntime, *fakeProgram, deployment.Request) {
	t.Helper()
	program := filepath.Join(t.TempDir(), "aries-bridge")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	runtime, err := NewProcessRuntime(state, map[string]string{"/usr/local/bin/aries-bridge": program})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeProgram{alive: map[int]bool{}, lingering: map[int]bool{}}
	runtime.start, runtime.processStart, runtime.signalGroup = fake.start, fake.processStart, fake.signal
	request := deployment.Request{
		Name: "aries-bridge-abc", Image: "example.test/bridge:1", Workdir: filepath.Join(state, "bridge-run-1"),
		Entrypoint: []string{"/usr/local/bin/aries-bridge"}, Args: []string{"config.json"}, ServicePort: 8443,
		Labels: map[string]string{"aries.managed": "true", "aries.component": "bridge", "aries.run": "run-1"},
	}
	return runtime, fake, request
}

func TestProcessRuntimeRunsTheMappedProgramInItsWorkdir(t *testing.T) {
	runtime, fake, request := newTestProcessRuntime(t)
	t.Setenv("DEEPSEEK_API_KEY", "must-not-reach-the-bridge")
	id, err := runtime.Create(context.Background(), request)
	if err != nil || id != request.Name {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if info, err := os.Stat(request.Workdir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("workdir: %v, %v", info, err)
	}
	if err := runtime.Validate(context.Background(), id, request, [][]byte{[]byte("absent")}); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	_ = writer.WriteHeader(&tar.Header{Name: "config.json", Mode: 0o600, Size: 2, Typeflag: tar.TypeReg})
	_, _ = writer.Write([]byte("{}"))
	_ = writer.Close()
	if err := runtime.UploadArchive(context.Background(), id, request.Workdir, &archive); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(request.Workdir, "config.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged config: %v, %v", info, err)
	}
	if err := runtime.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(fake.starts) != 1 || fake.starts[0][1] != "config.json" || fake.dirs[0] != request.Workdir {
		t.Fatalf("starts = %q in %q", fake.starts, fake.dirs)
	}
	for _, entry := range fake.envs[0] {
		if strings.HasPrefix(entry, "DEEPSEEK_API_KEY=") {
			t.Fatal("the process runtime passed Runner's credentials on")
		}
	}
	if running, err := runtime.Running(context.Background(), id); err != nil || !running {
		t.Fatalf("Running = %v, %v", running, err)
	}
	if address, err := runtime.Address(context.Background(), id, 8443); err != nil || address != "127.0.0.1:8443" {
		t.Fatalf("Address = %q, %v", address, err)
	}
	// Harnesses run in task namespaces, whose gateway reaches the run's loopback.
	if address, err := runtime.TaskAddress(context.Background(), id, 40123); err != nil || address != "10.0.3.2:40123" {
		t.Fatalf("TaskAddress = %q, %v", address, err)
	}
}

func TestProcessRuntimeConfinesTransfersToItsWorkdir(t *testing.T) {
	runtime, _, request := newTestProcessRuntime(t)
	id, err := runtime.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(request.Workdir, "evidence", "sandbox-1")
	if err := os.MkdirAll(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "tool-calls.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stream, info, err := runtime.DownloadArchive(context.Background(), id, filepath.Join(evidence, "tool-calls.jsonl"))
	if err != nil || info.Size != 3 || !info.Mode.IsRegular() {
		t.Fatalf("DownloadArchive = %+v, %v", info, err)
	}
	header, err := tar.NewReader(stream).Next()
	if err != nil || header.Name != "tool-calls.jsonl" || header.Size != 3 {
		t.Fatalf("archive entry = %+v, %v", header, err)
	}
	if _, _, err := runtime.DownloadArchive(context.Background(), id, filepath.Join(evidence, "missing")); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("missing evidence = %v", err)
	}
	if _, _, err := runtime.DownloadArchive(context.Background(), id, "/etc/passwd"); err == nil {
		t.Fatal("downloaded a file outside the workdir")
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	_ = writer.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg})
	_, _ = writer.Write([]byte("x"))
	_ = writer.Close()
	if err := runtime.UploadArchive(context.Background(), id, request.Workdir, &archive); err == nil {
		t.Fatal("extracted an entry outside its destination")
	}
	if err := runtime.UploadArchive(context.Background(), id, "/tmp", bytes.NewReader(nil)); err == nil {
		t.Fatal("uploaded outside the workdir")
	}
}

func TestProcessRuntimeStopEndsTheGroupRemovesTheWorkdirAndIsRepeatable(t *testing.T) {
	runtime, fake, request := newTestProcessRuntime(t)
	id, err := runtime.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.signals, []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("signals = %v", fake.signals)
	}
	if _, err := os.Stat(request.Workdir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workdir remains: %v", err)
	}
	if err := runtime.Stop(context.Background(), id); err != nil {
		t.Fatalf("repeated Stop = %v", err)
	}
	if running, err := runtime.Running(context.Background(), id); err != nil || running {
		t.Fatalf("Running after Stop = %v, %v", running, err)
	}
}

func TestProcessRuntimeStopWaitsForTheWholeGroup(t *testing.T) {
	runtime, fake, request := newTestProcessRuntime(t)
	id, err := runtime.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// The bridge exits on SIGTERM, but a process it started stays until SIGKILL.
	fake.mu.Lock()
	fake.lingering[7001] = true
	fake.mu.Unlock()
	if err := runtime.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.signals, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) || fake.lingering[7001] {
		t.Fatalf("signals = %v, lingering = %v", fake.signals, fake.lingering)
	}
}

func TestProcessRuntimeEndsAStartItCannotInspect(t *testing.T) {
	runtime, fake, request := newTestProcessRuntime(t)
	id, err := runtime.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	fake.inspect = errors.New("transient /proc error")
	if err := runtime.Start(context.Background(), id); err == nil || !strings.Contains(err.Error(), "transient") {
		t.Fatalf("Start = %v", err)
	}
	if !slices.Equal(fake.signals, []syscall.Signal{syscall.SIGTERM}) || fake.alive[7001] {
		t.Fatalf("the uninspected process was not ended: signals %v", fake.signals)
	}
}

func TestProcessRuntimeRefusesWhatItCannotProvide(t *testing.T) {
	cpu := 1.0
	for name, change := range map[string]func(*deployment.Request){
		"unmapped program": func(r *deployment.Request) { r.Entrypoint = []string{"/bin/sh"} },
		"relative workdir": func(r *deployment.Request) { r.Workdir = "bridge" },
		"mount": func(r *deployment.Request) {
			r.Mounts = []deployment.Mount{{Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}}
		},
		"environment":     func(r *deployment.Request) { r.Env = []string{"A=b"} },
		"cpu limit":       func(r *deployment.Request) { r.CPU = &cpu },
		"no service port": func(r *deployment.Request) { r.ServicePort = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			runtime, _, request := newTestProcessRuntime(t)
			change(&request)
			if id, err := runtime.Create(context.Background(), request); err == nil || id != "" {
				t.Fatalf("Create = %q, %v", id, err)
			}
			if _, err := os.Stat(request.Workdir); filepath.IsAbs(request.Workdir) && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workdir created for a refused request: %v", err)
			}
		})
	}
	runtime, _, request := newTestProcessRuntime(t)
	if _, err := runtime.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Create(context.Background(), request); err == nil {
		t.Fatal("Create reused an existing workdir")
	}
}
