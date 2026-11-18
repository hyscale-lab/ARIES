package apptainer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestTaskLoopbackGivesEveryTaskItsOwnAddress(t *testing.T) {
	for n, want := range map[uint32]string{
		1: "127.1.0.1", 255: "127.1.0.255", 256: "127.1.1.0", 65536: "127.2.0.0", 254<<16 - 1: "127.254.255.255",
	} {
		if got, err := taskLoopback(n); err != nil || got != want {
			t.Errorf("taskLoopback(%d) = %q, %v; want %q", n, got, err, want)
		}
	}
	for _, n := range []uint32{0, 254 << 16} {
		if got, err := taskLoopback(n); err == nil {
			t.Errorf("taskLoopback(%d) = %q, want an error", n, got)
		}
	}
}

func TestEnteredRunsTheProgramThroughNsenter(t *testing.T) {
	args := []string{"exec", "instance://x", "true"}
	if program, argv := entered(nil, "apptainer", args); program != "apptainer" || !slices.Equal(argv, args) {
		t.Fatalf("without a namespace = %q %q", program, argv)
	}
	enter := []string{"nsenter", "--user", "--net", "--preserve-credentials", "--target", "4242"}
	program, argv := entered(enter, "apptainer", args)
	want := []string{"--user", "--net", "--preserve-credentials", "--target", "4242", "apptainer", "exec", "instance://x", "true"}
	if program != "nsenter" || !slices.Equal(argv, want) {
		t.Fatalf("in a namespace = %q %q", program, argv)
	}
	if enter[len(enter)-1] != "4242" || len(enter) != 6 {
		t.Fatalf("entered changed its input: %q", enter)
	}
}

// writeTaskNamespace records a task namespace whose holder is pid, with the
// start time fakeProcesses reports for it.
func writeTaskNamespace(t *testing.T, stateDir, name string, pid int) taskNamespace {
	t.Helper()
	current := taskNamespace{Name: name, Runtime: "aries-sandbox-1", HolderPID: pid, HolderStart: uint64(pid) * 10, Network: "net:[4026532100]", Loopback: "127.1.0.7"}
	raw, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, name, namespaceFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return current
}

func TestRuntimesOfATaskNamespaceRunApptainerInside(t *testing.T) {
	manager, fake, processes := newTestManager(t, Options{})
	writeTaskNamespace(t, manager.options.StateDir, "ns-abc", 4242)
	processes.alive[4242] = true
	enter := []string{"nsenter", "--user", "--net", "--preserve-credentials", "--target", "4242"}
	request := testRequest()
	request.Placement.AttachmentID = taskNamespacePrefix + "ns-abc"
	request.StorageMB = 64
	ctx := context.Background()
	id, err := manager.Create(ctx, request)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := manager.Validate(ctx, id, request, nil); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := manager.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/true"}); err != nil || result.ExitCode != 0 {
		t.Fatalf("Exec = %+v, %v", result, err)
	}
	if err := manager.UploadArchive(ctx, id, "/opt", bytes.NewReader(nil)); err != nil {
		t.Fatalf("UploadArchive: %v", err)
	}
	// DownloadArchive first stats its source.
	fake.respond = func(_ context.Context, args []string, _ []byte, stdout, stderr io.Writer) (bool, error) {
		index := slices.Index(args, "aries-exec")
		if index < 0 || args[index+4] != "stat" {
			return false, nil
		}
		_, _ = io.WriteString(stdout, "7 81a4\n")
		_, err := fmt.Fprintf(stderr, "\x1eARIES_EXEC_EXIT_%s=0\x1f", args[index+1])
		return true, err
	}
	if stream, _, err := manager.DownloadArchive(ctx, id, "/opt/file"); err != nil {
		t.Fatalf("DownloadArchive: %v", err)
	} else {
		_, _ = io.ReadAll(stream)
		_ = stream.Close()
	}
	fake.respond = nil
	if err := manager.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	inside, outside := 0, 0
	for index, call := range fake.calls {
		switch {
		case call[0] == "exec" || len(call) > 1 && call[0] == "instance" && call[1] == "start":
			if !slices.Equal(fake.entered[index], enter) {
				t.Errorf("apptainer %q ran outside the task namespace: %q", call, fake.entered[index])
			}
			inside++
		default:
			// Listing, stopping and making overlays work from outside.
			if fake.entered[index] != nil {
				t.Errorf("apptainer %q entered the task namespace", call)
			}
			outside++
		}
	}
	// The instance start, faked, the privileges check, the command, the upload,
	// and the download's stat and tar ran inside.
	if inside != 7 || outside < 3 {
		t.Fatalf("calls inside = %d, outside = %d: %q", inside, outside, fake.calls)
	}
	if len(processes.entered) != 1 || !slices.Equal(processes.entered[0], enter) {
		t.Fatalf("the started command was launched with %q", processes.entered)
	}
}

func TestATaskNamespaceThatEndedIsNotEntered(t *testing.T) {
	manager, fake, processes := newTestManager(t, Options{})
	writeTaskNamespace(t, manager.options.StateDir, "ns-abc", 4242)
	processes.alive[4242] = true
	request := testRequest()
	request.Placement.AttachmentID = taskNamespacePrefix + "ns-abc"
	ctx := context.Background()
	id, err := manager.Create(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// A holder that exited, then a PID the kernel gave to a later process.
	delete(processes.alive, 4242)
	if _, err := manager.Exec(ctx, id, core.Command{Path: "/bin/true"}); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("Exec after the holder exited = %v", err)
	}
	reused := writeTaskNamespace(t, manager.options.StateDir, "ns-abc", 4242)
	reused.HolderStart++
	raw, err := json.Marshal(reused)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.options.StateDir, "ns-abc", namespaceFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	processes.alive[4242] = true
	calls := len(fake.callsWith("exec"))
	if _, err := manager.Exec(ctx, id, core.Command{Path: "/bin/true"}); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("Exec with a reused PID = %v", err)
	}
	if len(fake.callsWith("exec")) != calls {
		t.Fatal("apptainer ran for a namespace that ended")
	}
	other := testRequest()
	other.Name = "aries-task-def456"
	other.Placement.AttachmentID = taskNamespacePrefix + "ns-missing"
	if _, err := manager.Create(ctx, other); err == nil || len(fake.callsWith("instance", "start")) != 1 {
		t.Fatalf("Create in a namespace without a record = %v", err)
	}
	if _, err := os.Stat(manager.runtimeDir(other.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused Create left its directory: %v", err)
	}
}

// fakeSlirp answers slirp4netns's API on a Unix socket, keeping its forwards,
// with the replies of slirp4netns 1.2.1: a list is not wrapped in "return".
type fakeSlirp struct {
	mu       sync.Mutex
	forwards []slirpForward
	requests []string
	refuse   string
}

func serveFakeSlirp(t *testing.T, path string) *fakeSlirp {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	slirp := &fakeSlirp{}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			raw, _ := io.ReadAll(connection)
			var request struct {
				Execute   string       `json:"execute"`
				Arguments slirpForward `json:"arguments"`
			}
			_ = json.Unmarshal(raw, &request)
			slirp.mu.Lock()
			slirp.requests = append(slirp.requests, request.Execute)
			var reply any
			switch {
			case request.Execute == "list_hostfwd":
				reply = map[string]any{"entries": slices.Clone(slirp.forwards)}
			case request.Execute == "add_hostfwd" && slirp.refuse != "":
				reply = map[string]any{"error": map[string]any{"desc": slirp.refuse}}
			case request.Execute == "add_hostfwd":
				slirp.forwards = append(slirp.forwards, request.Arguments)
				reply = map[string]any{"return": map[string]any{"id": len(slirp.forwards)}}
			default:
				reply = map[string]any{"error": map[string]any{"desc": "unknown request"}}
			}
			slirp.mu.Unlock()
			_ = json.NewEncoder(connection).Encode(reply)
			_ = connection.Close()
		}
	}()
	return slirp
}

func TestAddressForwardsATaskPortThroughSlirp(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{})
	namespace := writeTaskNamespace(t, manager.options.StateDir, "ns-abc", 4242)
	slirp := serveFakeSlirp(t, filepath.Join(manager.options.StateDir, "ns-abc", slirpAPIFile))
	ctx := context.Background()
	for range 2 {
		if address, err := forwardPort(ctx, manager.options.StateDir, namespace, 8642); err != nil || address != "127.1.0.7:8642" {
			t.Fatalf("forwardPort = %q, %v", address, err)
		}
	}
	slirp.mu.Lock()
	if !slices.Equal(slirp.requests, []string{"list_hostfwd", "add_hostfwd", "list_hostfwd"}) {
		t.Fatalf("requests = %q", slirp.requests)
	}
	want := slirpForward{Proto: "tcp", HostAddr: "127.1.0.7", HostPort: 8642, GuestAddr: taskGuest, GuestPort: 8642}
	if len(slirp.forwards) != 1 || slirp.forwards[0] != want {
		t.Fatalf("forwards = %+v", slirp.forwards)
	}
	slirp.refuse = "bind: address already in use"
	slirp.mu.Unlock()
	if _, err := forwardPort(ctx, manager.options.StateDir, namespace, 18789); err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("a refused forward = %v", err)
	}
}

func TestAddressDoesNotReuseAForwardToAnotherGuest(t *testing.T) {
	manager, _, _ := newTestManager(t, Options{})
	namespace := writeTaskNamespace(t, manager.options.StateDir, "ns-abc", 4242)
	slirp := serveFakeSlirp(t, filepath.Join(manager.options.StateDir, "ns-abc", slirpAPIFile))
	// Another forward holds the task's address and port, to another guest
	// address, so slirp4netns refuses a second.
	slirp.mu.Lock()
	slirp.forwards = []slirpForward{{Proto: "tcp", HostAddr: "127.1.0.7", HostPort: 8642, GuestAddr: "10.0.3.101", GuestPort: 8642}}
	slirp.refuse = "bind: address already in use"
	slirp.mu.Unlock()
	if address, err := forwardPort(context.Background(), manager.options.StateDir, namespace, 8642); err == nil {
		t.Fatalf("forwardPort reused a forward to another guest: %q", address)
	}
}

func TestASlirpCallStopsWhenItsCallerCancels(t *testing.T) {
	path := filepath.Join(shortTempDir(t), slirpAPIFile)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	// A slirp4netns that takes the whole request and never answers.
	received := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.ReadAll(connection)
		close(received)
		<-release
	}()
	// Cancelled once the request is in, so the call is waiting for the reply;
	// the timeout bounds a failure.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-received:
		case <-time.After(5 * time.Second):
		}
		cancel()
	}()
	var reply map[string]any
	err = callSlirp(ctx, path, map[string]string{"execute": "list_hostfwd"}, &reply)
	select {
	case <-received:
	default:
		t.Fatal("the fake slirp4netns never received the whole request")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callSlirp = %v, want context.Canceled", err)
	}
}

func TestAddressOfATaskRuntimeIsItsLoopback(t *testing.T) {
	manager, _, processes := newTestManager(t, Options{})
	writeTaskNamespace(t, manager.options.StateDir, "ns-abc", 4242)
	serveFakeSlirp(t, filepath.Join(manager.options.StateDir, "ns-abc", slirpAPIFile))
	processes.alive[4242] = true
	request := testRequest()
	request.Placement.AttachmentID = taskNamespacePrefix + "ns-abc"
	request.ServicePort = 8642
	id, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if address, err := manager.Address(context.Background(), id, 8642); err != nil || address != "127.1.0.7:8642" {
		t.Fatalf("Address = %q, %v", address, err)
	}
}

// fakeProcRoot builds a /proc whose processes are in the given networks; a
// process mapped to "" has no readable namespace, like another user's.
func fakeProcRoot(t *testing.T, networks map[int]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, network := range networks {
		directory := filepath.Join(root, strconv.Itoa(pid), "ns")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if network != "" {
			if err := os.Symlink(network, filepath.Join(directory, "net")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestProcessesInNetworkReadsEachProcess(t *testing.T) {
	root := fakeProcRoot(t, map[int]string{1: "net:[100]", 22: "net:[200]", 333: "net:[100]", 4444: ""})
	members, err := processesInNetwork(root, "net:[100]")
	slices.Sort(members)
	if err != nil || !slices.Equal(members, []int{1, 333}) {
		t.Fatalf("processesInNetwork = %v, %v", members, err)
	}
}

func TestStoppingATaskNamespaceWaitsForItsRuntimes(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	directory := filepath.Join(stateDir, "ns-abc")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	root := fakeProcRoot(t, map[int]string{4242: "net:[100]", 5000: "net:[100]", 6000: "net:[200]"})
	releaseInput, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = releaseInput.Close(); _ = release.Close() })
	slirpInput, slirpExit, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slirpInput.Close(); _ = slirpExit.Close() })
	namespace := &runningTaskNamespace{
		record:    taskNamespace{Name: "ns-abc", HolderPID: 4242, Network: "net:[100]"},
		directory: directory, procRoot: root, release: release, slirpExit: slirpExit,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := namespace.Stop(ctx); err == nil || !strings.Contains(err.Error(), "[5000]") {
		t.Fatalf("Stop with a runtime's process left = %v", err)
	}
	if _, err := os.Stat(directory); err != nil || namespace.release == nil {
		t.Fatalf("a refused Stop ended the namespace: %v", err)
	}
	// The runtime's process leaves; then the holder ends once its input closes.
	_ = os.RemoveAll(filepath.Join(root, "5000"))
	_ = os.RemoveAll(filepath.Join(root, "4242"))
	if err := namespace.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) || namespace.release != nil || namespace.slirpExit != nil {
		t.Fatalf("after Stop: %v", err)
	}
	if err := namespace.Stop(context.Background()); err != nil {
		t.Fatalf("a repeated Stop: %v", err)
	}
}

func TestEndingATaskNamespaceKeepsToTheCallersDeadline(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "ns-abc")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	// A slirp4netns that does not exit when its exit pipe closes.
	slirp := exec.Command("sleep", "30")
	if err := slirp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = slirp.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- slirp.Wait() }()
	exitInput, slirpExit, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exitInput.Close(); _ = slirpExit.Close() })
	namespace := &runningTaskNamespace{
		record: taskNamespace{Name: "ns-abc"}, directory: directory, procRoot: "/proc",
		slirp: slirp.Process, slirpDone: done, slirpExit: slirpExit,
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := namespace.end(expired); err == nil || time.Since(started) > time.Second {
		t.Fatalf("end with an expired deadline = %v after %v", err, time.Since(started))
	}
	if _, err := os.Stat(directory); err != nil || namespace.slirpExit == nil {
		t.Fatalf("the record or slirp4netns went before its exit was confirmed: %v", err)
	}
	// Once slirp4netns has exited, a later end removes the record.
	_ = slirp.Process.Kill()
	if err := namespace.end(context.Background()); err != nil {
		t.Fatalf("end after slirp4netns exited: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) || namespace.slirpExit != nil {
		t.Fatalf("after end: %v", err)
	}
}

func TestCheckStateDirFitsATasksSocket(t *testing.T) {
	// <state_dir>/ns-<16 hex digits>/slirp.sock takes 31 bytes after the
	// directory, of the 107 a Unix socket address holds.
	fits := "/" + strings.Repeat("s", 75)
	if err := CheckStateDir(fits); err != nil {
		t.Fatalf("CheckStateDir of %d bytes: %v", len(fits), err)
	}
	if err := CheckStateDir(fits + "s"); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("CheckStateDir of %d bytes = %v", len(fits)+1, err)
	}
}

func TestATooLongStateDirectoryIsRefusedBeforeAnything(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), strings.Repeat("d", 100))
	if _, err := startTaskNamespace(context.Background(), taskNamespaceRequest{stateDir: stateDir, loopback: "127.1.0.1"}); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("startTaskNamespace = %v", err)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the refused namespace created %s: %v", stateDir, err)
	}
}
