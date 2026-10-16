package apptainer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

func TestRunEnvironmentRequiresItsNamespaceAndResolvesServicesOnLoopback(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{})
	environment := manager.NewRunEnvironment("run-1")
	network := "net:[4026532001]"
	environment.current = func() (string, error) { return network, nil }
	placement, err := environment.Start(context.Background())
	if err != nil || placement.AttachmentID != "run-namespace:net:[4026532001]" {
		t.Fatalf("Start = %+v, %v", placement, err)
	}
	task := environment.NewTaskEnvironment()
	request := deployment.TaskEnvironmentRequest{RuntimeName: "aries-task-1"}
	request.RunID = "run-1"
	request.Environment.Services = core.TaskServices{SearchPort: 8000, MCP: []core.TaskMCPService{{Name: "toolathlon", Port: 10086, Path: "/sse", Transport: "sse"}}}
	connectivity, err := task.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if connectivity.SearchURL != "http://127.0.0.1:8000" || len(connectivity.MCPServers) != 1 || connectivity.MCPServers[0].URL != "http://127.0.0.1:10086/sse" || connectivity.Placement != placement {
		t.Fatalf("connectivity = %+v", connectivity)
	}
	network = "net:[4026532999]"
	if err := task.Validate(context.Background()); err == nil {
		t.Fatal("Validate accepted a process that left its namespace")
	}
	if err := environment.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := manager.NewRunEnvironment("run-2")
	other.current = func() (string, error) { return "", errors.New("not in a run namespace") }
	if _, err := other.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded outside a run namespace")
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

// fakeProgram records process runtime launches.
type fakeProgram struct {
	mu      sync.Mutex
	starts  [][]string
	dirs    []string
	envs    [][]string
	alive   map[int]bool
	signals []syscall.Signal
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
	return uint64(pid), f.alive[pid], nil
}

func (f *fakeProgram) signal(pgid int, signal syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, signal)
	delete(f.alive, pgid)
	return nil
}

func newTestProcessRuntime(t *testing.T) (*ProcessRuntime, *fakeProgram, deployment.Request) {
	t.Helper()
	program := filepath.Join(t.TempDir(), "aries-bridge")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewProcessRuntime(map[string]string{"/usr/local/bin/aries-bridge": program})
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeProgram{alive: map[int]bool{}}
	runtime.start, runtime.processStart, runtime.signalGroup = fake.start, fake.processStart, fake.signal
	request := deployment.Request{
		Name: "aries-bridge-abc", Image: "example.test/bridge:1", Workdir: filepath.Join(t.TempDir(), "state", "bridge-run-1"),
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
	if address, err := runtime.TaskAddress(context.Background(), id, 40123); err != nil || address != "127.0.0.1:40123" {
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
