package apptainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const testImage = "example.test/task:1"

// fakeApptainer keeps a table of instances and answers each invocation as
// apptainer would, unless respond handles it first.
type fakeApptainer struct {
	mu        sync.Mutex
	calls     [][]string
	instances map[string]instanceInfo
	nextPID   int
	respond   func(ctx context.Context, args []string, stdin []byte, stdout, stderr io.Writer) (bool, error)
}

func (f *fakeApptainer) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	var input []byte
	if stdin != nil {
		input, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(args))
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		if handled, err := respond(ctx, args, input, stdout, stderr); handled {
			return err
		}
	}
	return f.answer(args, stdout, stderr)
}

func (f *fakeApptainer) answer(args []string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(args) > 2 && args[0] == "instance" && args[1] == "start":
		f.nextPID++
		name := args[len(args)-1]
		f.instances[name] = instanceInfo{Name: name, PID: 1000 + f.nextPID, Image: args[len(args)-2]}
	case len(args) > 2 && args[0] == "instance" && args[1] == "list":
		listing := struct {
			Instances []instanceInfo `json:"instances"`
		}{Instances: []instanceInfo{}}
		if instance, ok := f.instances[args[len(args)-1]]; ok {
			listing.Instances = append(listing.Instances, instance)
		}
		return json.NewEncoder(stdout).Encode(listing)
	case len(args) > 2 && args[0] == "instance" && args[1] == "stop":
		delete(f.instances, args[len(args)-1])
	case len(args) > 2 && args[0] == "overlay" && args[1] == "create":
		return os.WriteFile(args[len(args)-1], []byte("overlay"), 0o600)
	case len(args) > 3 && args[0] == "build":
		return os.MkdirAll(filepath.Join(args[len(args)-2], "bin"), 0o700)
	case len(args) > 0 && args[0] == "exec":
		return fakeExec(args, stdout, stderr)
	}
	return nil
}

// fakeExec runs the wrapper's contract: the command's output, then the exit
// trailer on stderr. A command named /bin/exit7 exits 7.
func fakeExec(args []string, stdout, stderr io.Writer) error {
	index := slices.Index(args, "aries-exec")
	if index < 0 {
		if slices.Contains(args, "/proc/self/status") {
			_, err := io.WriteString(stdout, "Name:\tcat\nNoNewPrivs:\t1\n")
			return err
		}
		return nil
	}
	token, command := args[index+2], args[index+5]
	code := 0
	if command == "/bin/exit7" {
		code = 7
	}
	_, _ = io.WriteString(stdout, "out\n")
	_, _ = io.WriteString(stderr, "err\n")
	_, _ = fmt.Fprintf(stderr, "\x1eARIES_EXEC_EXIT_%s=%d\x1f", token, code)
	if code != 0 {
		return exitError(code)
	}
	return nil
}

func (f *fakeApptainer) callsWith(prefix ...string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched [][]string
	for _, call := range f.calls {
		if len(call) >= len(prefix) && slices.Equal(call[:len(prefix)], prefix) {
			matched = append(matched, call)
		}
	}
	return matched
}

type exitError int

func (e exitError) Error() string { return "exit status " + fmt.Sprint(int(e)) }
func (e exitError) ExitCode() int { return int(e) }

// fakeProcesses stands in for the launcher, /proc and signals.
type fakeProcesses struct {
	mu       sync.Mutex
	launches [][]string
	alive    map[int]bool
	signals  []syscall.Signal
}

func (p *fakeProcesses) Launch(args []string, _, _ *os.File) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.launches = append(p.launches, slices.Clone(args))
	pid := 4200 + len(p.launches)
	p.alive[pid] = true
	return pid, nil
}

func (p *fakeProcesses) start(pid int) (uint64, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return uint64(pid) * 10, p.alive[pid], nil
}

func (p *fakeProcesses) signal(pgid int, signal syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, signal)
	delete(p.alive, pgid)
	return nil
}

func newTestManager(t *testing.T, options Options) (*Manager, *fakeApptainer, *fakeProcesses) {
	t.Helper()
	options.StateDir = filepath.Join(t.TempDir(), "state")
	options.ImageDir = t.TempDir()
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootfsPath(options.ImageDir, testImage), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeApptainer{instances: map[string]instanceInfo{}}
	processes := &fakeProcesses{alive: map[int]bool{}}
	manager.apptainer, manager.launcher = fake, processes
	manager.processStart, manager.signalGroup = processes.start, processes.signal
	return manager, fake, processes
}

func testRequest() deployment.Request {
	return deployment.Request{
		Name: "aries-task-abc123", Image: testImage, Workdir: "/app",
		Env: []string{"A=b'c", "B=$HOME"}, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"},
		Labels:    map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run-1", "aries.task": "task-1"},
		Placement: core.RuntimePlacement{AttachmentID: "run-namespace"},
		Mounts:    []deployment.Mount{{Source: "/srv/data", Target: "/data", ReadOnly: true}},
		Init:      true, NoNewPrivileges: true,
	}
}

func TestCreateStartsAContainedInstanceUnderAPrivateRecord(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	request := testRequest()
	id, err := manager.Create(context.Background(), request)
	if err != nil || id != request.Name {
		t.Fatalf("Create = %q, %v", id, err)
	}
	starts := fake.callsWith("instance", "start")
	if len(starts) != 1 {
		t.Fatalf("instance starts = %v", starts)
	}
	overlay := filepath.Join(manager.options.OverlayDir, request.Name+".overlay")
	share := filepath.Join(manager.runtimeDir(id), "share")
	want := []string{"instance", "start", "--containall", "--fakeroot", "--overlay", overlay, "--bind", share + ":/.aries:ro", "--bind", "/srv/data:/data:ro", rootfsPath(manager.options.ImageDir, testImage), request.Name}
	if !slices.Equal(starts[0], want) {
		t.Fatalf("instance start = %q, want %q", starts[0], want)
	}
	if info, err := os.Stat(overlay); err != nil || !info.IsDir() {
		t.Fatalf("overlay directory: %v", err)
	}
	environment := filepath.Join(share, environmentFile)
	info, err := os.Stat(environment)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("environment file mode: %v, %v", info, err)
	}
	content, _ := os.ReadFile(environment)
	if string(content) != "A='b'\\''c'\nB='$HOME'\n" {
		t.Fatalf("environment file = %q", content)
	}
	current, _, err := manager.loadRecord(id)
	if err != nil || current.InstancePID != 1001 || current.MainPID != 0 {
		t.Fatalf("record = %+v, %v", current, err)
	}
}

func TestCreateRejectsWhatApptainerCannotProvideBeforeAnyEffect(t *testing.T) {
	cpu := 1.0
	for name, change := range map[string]func(*deployment.Request){
		"cpu limit":       func(r *deployment.Request) { r.CPU = &cpu },
		"network aliases": func(r *deployment.Request) { r.NetworkAliases = []string{"search"} },
		"unsafe name":     func(r *deployment.Request) { r.Name = "../escape" },
		"option name":     func(r *deployment.Request) { r.Name = "-x" },
		"no attachment":   func(r *deployment.Request) { r.Placement = core.RuntimePlacement{} },
		"mount with ':'":  func(r *deployment.Request) { r.Mounts = []deployment.Mount{{Source: "/a:b", Target: "/c"}} },
		"unprepared":      func(r *deployment.Request) { r.Image = "example.test/other:1" },
		"share target":    func(r *deployment.Request) { r.Mounts = []deployment.Mount{{Source: "/srv", Target: "/.aries"}} },
		"empty command":   func(r *deployment.Request) { r.Entrypoint, r.Args = []string{}, nil },
	} {
		t.Run(name, func(t *testing.T) {
			manager, fake, _ := newTestManager(t, Options{})
			request := testRequest()
			change(&request)
			if id, err := manager.Create(context.Background(), request); err == nil || id != "" {
				t.Fatalf("Create = %q, %v", id, err)
			}
			if len(fake.calls) != 0 {
				t.Fatalf("calls = %v", fake.calls)
			}
		})
	}
}

func TestCreateAcceptsUnenforcedLimitsOnlyWhenConfigured(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{UnenforcedLimits: true})
	cpu, memory := 2.0, 1024
	request := testRequest()
	request.CPU, request.MemoryMB = &cpu, &memory
	if _, err := manager.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

func TestCreateBoundsStorageWithAFixedSizeOverlayImage(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	request := testRequest()
	request.StorageMB = 512
	if _, err := manager.Create(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(manager.options.OverlayDir, request.Name+".overlay.img")
	creates := fake.callsWith("overlay", "create")
	if len(creates) != 1 || !slices.Equal(creates[0], []string{"overlay", "create", "--fakeroot", "--sparse", "--size", "512", overlay}) {
		t.Fatalf("overlay create = %v", creates)
	}
	if start := fake.callsWith("instance", "start"); len(start) != 1 || start[0][5] != overlay {
		t.Fatalf("instance start = %v", start)
	}
}

func TestCreateRemovesItsFilesWhenNoInstanceStarted(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	fake.respond = func(_ context.Context, args []string, _ []byte, _, _ io.Writer) (bool, error) {
		if args[0] == "instance" && args[1] == "start" {
			return true, errors.New("starter failed")
		}
		return false, nil
	}
	request := testRequest()
	id, err := manager.Create(context.Background(), request)
	if err == nil || id != "" || errors.Is(err, deployment.ErrAllocationUnconfirmed) {
		t.Fatalf("Create = %q, %v", id, err)
	}
	for _, path := range []string{manager.runtimeDir(request.Name), filepath.Join(manager.options.OverlayDir, request.Name+".overlay")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains: %v", path, err)
		}
	}
}

func TestCreateKeepsTheIdentityOfAnInstanceThatStartedDespiteAnError(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	fake.respond = func(_ context.Context, args []string, _ []byte, stdout, stderr io.Writer) (bool, error) {
		if args[0] == "instance" && args[1] == "start" {
			_ = fake.answer(args, stdout, stderr)
			return true, errors.New("lost response")
		}
		return false, nil
	}
	request := testRequest()
	id, err := manager.Create(context.Background(), request)
	if err == nil || id != request.Name {
		t.Fatalf("Create = %q, %v", id, err)
	}
	if current, _, err := manager.loadRecord(id); err != nil || current.InstancePID == 0 {
		t.Fatalf("record = %+v, %v", current, err)
	}
}

func TestCreateReportsAnUnresolvedAllocation(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	fake.respond = func(_ context.Context, args []string, _ []byte, _, _ io.Writer) (bool, error) {
		if args[0] == "instance" {
			return true, errors.New("apptainer unavailable")
		}
		return false, nil
	}
	id, err := manager.Create(context.Background(), testRequest())
	if id != "" || !errors.Is(err, deployment.ErrAllocationUnconfirmed) {
		t.Fatalf("Create = %q, %v", id, err)
	}
}

func TestValidateChecksOwnershipSecretsAndPrivileges(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	request := testRequest()
	id, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Validate(context.Background(), id, request, [][]byte{[]byte("not-present")}); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if err := manager.Validate(context.Background(), id, request, [][]byte{[]byte("b'\\''c")}); err == nil {
		t.Fatal("Validate accepted a secret in the environment file")
	}
	other := testRequest()
	other.Labels = map[string]string{"aries.run": "run-2"}
	if err := manager.Validate(context.Background(), id, other, nil); err == nil {
		t.Fatal("Validate accepted another run's labels")
	}
	fake.respond = func(_ context.Context, args []string, _ []byte, stdout, _ io.Writer) (bool, error) {
		if slices.Contains(args, "/proc/self/status") {
			_, err := io.WriteString(stdout, "NoNewPrivs:\t0\n")
			return true, err
		}
		return false, nil
	}
	if err := manager.Validate(context.Background(), id, request, nil); err == nil {
		t.Fatal("Validate accepted a runtime without no-new-privileges")
	}
	fake.mu.Lock()
	fake.instances[id] = instanceInfo{Name: id, PID: 99, Image: rootfsPath(manager.options.ImageDir, testImage)}
	fake.mu.Unlock()
	fake.respond = nil
	if err := manager.Validate(context.Background(), id, request, nil); err == nil {
		t.Fatal("Validate accepted an instance whose PID differs from the record")
	}
}

func TestStartRunsTheEntrypointOnceAndRunningFollowsIt(t *testing.T) {
	manager, _, processes := newTestManager(t, Options{})
	request := testRequest()
	id, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if running, err := manager.Running(context.Background(), id); err != nil || running {
		t.Fatalf("Running before Start = %v, %v", running, err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--cleanenv", "--pwd", "/app", "instance://" + id, "/bin/sh", "-c", startShell, "aries-start", "/.aries/env", "/bin/sleep", "infinity"}
	if len(processes.launches) != 1 || !slices.Equal(processes.launches[0], want) {
		t.Fatalf("launches = %q, want one %q", processes.launches, want)
	}
	if running, err := manager.Running(context.Background(), id); err != nil || !running {
		t.Fatalf("Running = %v, %v", running, err)
	}
	processes.mu.Lock()
	processes.alive = map[int]bool{}
	processes.mu.Unlock()
	if running, err := manager.Running(context.Background(), id); err != nil || running {
		t.Fatalf("Running after exit = %v, %v", running, err)
	}
}

func TestStartRunsTheRunscriptWithoutAnEntrypoint(t *testing.T) {
	manager, _, processes := newTestManager(t, Options{})
	request := testRequest()
	request.Entrypoint, request.Args, request.Workdir = nil, []string{"serve"}, ""
	id, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--cleanenv", "--pwd", "/", "instance://" + id, "/bin/sh", "-c", startShell, "aries-start", "/.aries/env", "/.singularity.d/runscript", "serve"}
	if !slices.Equal(processes.launches[0], want) {
		t.Fatalf("launch = %q, want %q", processes.launches[0], want)
	}
}

func TestStopEndsTheCommandAndInstanceRemovesFilesAndIsRepeatable(t *testing.T) {
	manager, fake, processes := newTestManager(t, Options{})
	request := testRequest()
	id, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(processes.signals, []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("signals = %v", processes.signals)
	}
	if stops := fake.callsWith("instance", "stop"); len(stops) != 1 || !slices.Equal(stops[0], []string{"instance", "stop", "--timeout", "5", id}) {
		t.Fatalf("instance stops = %v", stops)
	}
	for _, path := range []string{manager.runtimeDir(id), filepath.Join(manager.options.OverlayDir, id+".overlay")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s remains: %v", path, err)
		}
	}
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatalf("repeated Stop = %v", err)
	}
	if stops := fake.callsWith("instance", "stop"); len(stops) != 1 {
		t.Fatalf("repeated Stop stopped again: %v", stops)
	}
}

func TestStopReportsAnInstanceThatRemains(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	fake.respond = func(_ context.Context, args []string, _ []byte, _, _ io.Writer) (bool, error) {
		return args[0] == "instance" && args[1] == "stop", nil
	}
	if err := manager.Stop(context.Background(), id); err == nil || !strings.Contains(err.Error(), "remains") {
		t.Fatalf("Stop = %v", err)
	}
	if _, _, err := manager.loadRecord(id); err != nil {
		t.Fatalf("record removed although the instance remains: %v", err)
	}
}

func TestStopRefusesAnInstanceWithoutARecord(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	fake.instances["aries-task-foreign"] = instanceInfo{Name: "aries-task-foreign", PID: 7, Image: "/elsewhere.sif"}
	if err := manager.Stop(context.Background(), "aries-task-foreign"); err == nil {
		t.Fatal("Stop removed an instance it has no record of")
	}
	if len(fake.callsWith("instance", "stop")) != 0 {
		t.Fatal("Stop signalled a foreign instance")
	}
}

func TestExecStreamPreservesArgvEnvironmentAndExitCode(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	var environment, environmentPath string
	fake.respond = func(_ context.Context, args []string, _ []byte, _, _ io.Writer) (bool, error) {
		if index := slices.Index(args, "aries-exec"); index > 0 {
			environmentPath = filepath.Join(manager.runtimeDir(id), "share", filepath.Base(args[index+4]))
			content, err := os.ReadFile(environmentPath)
			environment = string(content)
			if err != nil {
				return true, err
			}
		}
		return false, nil
	}
	result, err := manager.Exec(context.Background(), id, core.Command{Path: "/bin/echo", Args: []string{"a b", ""}, Dir: "/work", Env: map[string]string{"K": "it's"}})
	if err != nil || result.ExitCode != 0 || result.Stdout != "out\n" || result.Stderr != "err\n" {
		t.Fatalf("Exec = %+v, %v", result, err)
	}
	execs := fake.callsWith("exec", "--cleanenv", "--pwd", "/app", "instance://"+id, "/bin/sh", "-c", execShell, "aries-exec")
	if len(execs) != 1 {
		t.Fatalf("exec calls = %q", execs)
	}
	call, index := execs[0], 8
	if !strings.HasPrefix(call[index+1], execStatePrefix) || call[index+3] != "/work" || !strings.HasPrefix(call[index+4], "/.aries/exec-") ||
		!slices.Equal(call[index+5:], []string{"/bin/echo", "a b", ""}) {
		t.Fatalf("exec call = %q", call)
	}
	if environment != "A='b'\\''c'\nB='$HOME'\nK='it'\\''s'\n" {
		t.Fatalf("exec environment = %q", environment)
	}
	if _, err := os.Stat(environmentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exec environment file remains: %v", err)
	}
	result, err = manager.Exec(context.Background(), id, core.Command{Path: "/bin/exit7", User: "0:0"})
	if err != nil || result.ExitCode != 7 {
		t.Fatalf("Exec exit 7 = %+v, %v", result, err)
	}
}

func TestExecRefusesAnotherUser(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	before := len(fake.calls)
	if _, err := manager.Exec(context.Background(), id, core.Command{Path: "/bin/true", User: "65532:65532"}); err == nil {
		t.Fatal("Exec accepted a non-root user")
	}
	if len(fake.calls) != before {
		t.Fatal("Exec ran apptainer for a refused command")
	}
}

func TestExecTimeoutTerminatesTheCommandGroup(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	fake.respond = func(ctx context.Context, args []string, _ []byte, _, _ io.Writer) (bool, error) {
		if slices.Contains(args, "aries-exec") {
			<-ctx.Done()
			return true, ctx.Err()
		}
		return slices.Contains(args, "aries-cancel"), nil
	}
	_, err = manager.Exec(context.Background(), id, core.Command{Path: "/bin/sleep", Args: []string{"60"}, Timeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec = %v", err)
	}
	cancels := 0
	for _, call := range fake.callsWith("exec", "--cleanenv") {
		if slices.Contains(call, "aries-cancel") {
			cancels++
		}
	}
	if cancels != 1 {
		t.Fatalf("termination helpers = %d", cancels)
	}
}

func TestDownloadArchiveSeparatesAMissingSource(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	fake.respond = func(_ context.Context, args []string, _ []byte, _, stderr io.Writer) (bool, error) {
		if index := slices.Index(args, "aries-exec"); index > 0 && args[index+5] == "stat" {
			_, _ = io.WriteString(stderr, "stat: cannot stat '/missing': No such file or directory\n")
			_, _ = fmt.Fprintf(stderr, "\x1eARIES_EXEC_EXIT_%s=1\x1f", args[index+2])
			return true, exitError(1)
		}
		return false, nil
	}
	if _, _, err := manager.DownloadArchive(context.Background(), id, "/missing"); !errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("DownloadArchive = %v", err)
	}
	if _, _, err := manager.DownloadArchive(context.Background(), "aries-task-gone", "/x"); err == nil || errors.Is(err, runner.ErrNotFound) {
		t.Fatalf("DownloadArchive of a missing runtime = %v", err)
	}
}

func TestUploadArchiveExtractsWithoutChangingOwners(t *testing.T) {
	manager, fake, _ := newTestManager(t, Options{})
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.UploadArchive(context.Background(), id, "/root/.hermes", strings.NewReader("tar")); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--cleanenv", "--pwd", "/", "instance://" + id, "tar", "-x", "-o", "-f", "-", "-C", "/root/.hermes"}
	if uploads := fake.callsWith("exec", "--cleanenv", "--pwd", "/", "instance://"+id, "tar"); len(uploads) != 1 || !slices.Equal(uploads[0], want) {
		t.Fatalf("upload = %v", uploads)
	}
}

func TestAddressServesOnlyThePublishedPort(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{})
	request := testRequest()
	request.ServicePort = 18789
	id, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if address, err := manager.Address(context.Background(), id, 18789); err != nil || address != "127.0.0.1:18789" {
		t.Fatalf("Address = %q, %v", address, err)
	}
	if _, err := manager.Address(context.Background(), id, 8443); err == nil {
		t.Fatal("Address resolved a port the runtime does not publish")
	}
}

func TestValidateBridgeTargetRequiresTheRunsLiveSandbox(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{})
	id, err := manager.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	target := core.BridgeTarget{SandboxID: id, RuntimeID: id, RunID: "run-1", TaskID: "task-1", Backend: "apptainer"}
	if err := manager.ValidateBridgeTarget(context.Background(), target); err == nil {
		t.Fatal("accepted a sandbox that was never started")
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := manager.ValidateBridgeTarget(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*core.BridgeTarget){
		"other run":     func(d *core.BridgeTarget) { d.RunID = "run-2" },
		"other backend": func(d *core.BridgeTarget) { d.Backend = "docker" },
		"other runtime": func(d *core.BridgeTarget) { d.RuntimeID = "aries-task-other" },
	} {
		changed := target
		change(&changed)
		if err := manager.ValidateBridgeTarget(context.Background(), changed); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestPullImagesUnpacksEachImageOnce(t *testing.T) {
	directory := t.TempDir()
	fake := &fakeApptainer{instances: map[string]instanceInfo{}}
	images := []string{"example.test/task:1", "example.test/task:1", "aries-local/hermes:1"}
	sources := map[string]string{"aries-local/hermes:1": "/images/hermes.sif"}
	if err := pullImages(context.Background(), fake, directory, images, sources); err != nil {
		t.Fatal(err)
	}
	builds := fake.callsWith("build", "--disable-cache", "--fix-perms", "--sandbox")
	if len(builds) != 2 || builds[0][5] != "docker://example.test/task:1" || builds[1][5] != "/images/hermes.sif" {
		t.Fatalf("builds = %q", builds)
	}
	for _, image := range []string{"example.test/task:1", "aries-local/hermes:1"} {
		destination := rootfsPath(directory, image)
		if info, err := os.Stat(filepath.Join(destination, "bin")); err != nil || !info.IsDir() {
			t.Fatalf("%s: unpacked image missing: %v", image, err)
		}
		if reference, err := os.ReadFile(destination + ".ref"); err != nil || string(reference) != image+"\n" {
			t.Fatalf("reference = %q, %v", reference, err)
		}
	}
	if err := pullImages(context.Background(), fake, directory, images[:1], nil); err != nil {
		t.Fatal(err)
	}
	if len(fake.callsWith("build")) != 2 {
		t.Fatal("an unpacked image was built again")
	}
	// A setup that stopped before recording an image records it next time and
	// leaves an existing record alone.
	taskImage := rootfsPath(directory, "example.test/task:1")
	if err := os.Remove(taskImage + ".ref"); err != nil {
		t.Fatal(err)
	}
	hermesImage := rootfsPath(directory, "aries-local/hermes:1")
	if err := os.WriteFile(hermesImage+".ref", []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pullImages(context.Background(), fake, directory, images, sources); err != nil {
		t.Fatal(err)
	}
	if reference, err := os.ReadFile(taskImage + ".ref"); err != nil || string(reference) != "example.test/task:1\n" {
		t.Fatalf("restored reference = %q, %v", reference, err)
	}
	if reference, err := os.ReadFile(hermesImage + ".ref"); err != nil || string(reference) != "kept\n" {
		t.Fatalf("existing reference = %q, %v", reference, err)
	}
	if len(fake.callsWith("build")) != 2 {
		t.Fatal("recording a reference built an image again")
	}
	if err := pullImages(context.Background(), fake, directory, []string{"no tag"}, nil); err == nil {
		t.Fatal("accepted an invalid reference")
	}
	if err := pullImages(context.Background(), fake, directory, []string{"x:1"}, map[string]string{"x:1": "x.sif"}); err == nil {
		t.Fatal("accepted a relative source")
	}
}

func TestParseProcStatReadsStartTimeAndZombies(t *testing.T) {
	line := "4242 (a) b)) S 1 4242 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10\n"
	if started, alive, err := parseProcStat([]byte(line)); err != nil || !alive || started != 987654 {
		t.Fatalf("parse = %d, %v, %v", started, alive, err)
	}
	zombie := strings.Replace(line, ") S ", ") Z ", 1)
	if _, alive, err := parseProcStat([]byte(zombie)); err != nil || alive {
		t.Fatalf("zombie = %v, %v", alive, err)
	}
}

func TestCheckVersionRequiresTheOldestVersionThatWorked(t *testing.T) {
	for version, accepted := range map[string]bool{"1.5.0\n": true, "1.10.1": true, "2.0.0": true, "1.2.2-1.el8\n": false, "1.4.5": false, "unknown": false} {
		fake := &fakeApptainer{instances: map[string]instanceInfo{}}
		fake.respond = func(_ context.Context, args []string, _ []byte, stdout, _ io.Writer) (bool, error) {
			_, err := io.WriteString(stdout, version)
			return true, err
		}
		err := (&Manager{apptainer: fake}).checkVersion(context.Background())
		if (err == nil) != accepted {
			t.Fatalf("version %q: %v", version, err)
		}
	}
}
