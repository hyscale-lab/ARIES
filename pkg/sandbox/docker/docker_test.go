package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

var (
	errCommand = errors.New("command failed")
	errCleanup = errors.New("cleanup failed")
)

type recordedCall struct {
	stdin []byte
	args  []string
}

type response func(context.Context, recordedCall) (commandResult, error)

type fakeCommandRunner struct {
	mu        sync.Mutex
	responses []response
	calls     []recordedCall
}

func (f *fakeCommandRunner) Run(ctx context.Context, stdin []byte, args ...string) (commandResult, error) {
	call := recordedCall{stdin: slices.Clone(stdin), args: slices.Clone(args)}
	f.mu.Lock()
	index := len(f.calls)
	f.calls = append(f.calls, call)
	if index >= len(f.responses) {
		f.mu.Unlock()
		return commandResult{exitCode: -1}, errors.New("unexpected fake Docker call")
	}
	respond := f.responses[index]
	f.mu.Unlock()
	return respond(ctx, call)
}

func (f *fakeCommandRunner) snapshot() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls...)
}

func result(stdout, stderr string, exitCode int, err error) response {
	return func(context.Context, recordedCall) (commandResult, error) {
		return commandResult{stdout: []byte(stdout), stderr: []byte(stderr), exitCode: exitCode}, err
	}
}

func successfulInspection(running bool) string {
	return `[{"State":{"Running":` + strconv.FormatBool(running) + `},"Config":{"WorkingDir":"/work","Labels":{"aries.managed":"true","aries.run":"run-1","aries.task":"task-1"}},"NetworkSettings":{"Networks":{"aries-net-fixedid":{}}},"Mounts":[{"Destination":"/opt/aries/bin/aries-exec-helper","RW":false},{"Destination":"/run/aries","RW":false}]}]`
}

func successfulNetworkInspection() string {
	return `[{"Labels":{"aries.managed":"true","aries.run":"run-1","aries.task":"task-1"}}]`
}

func successfulStartResponses() []response {
	return []response{
		result("network-id\n", "", 0, nil),
		result("container-id\n", "", 0, nil),
		result("container-id\n", "", 0, nil),
		result(successfulInspection(true), "", 0, nil),
		result(successfulNetworkInspection(), "", 0, nil),
	}
}

func successfulStopResponses(stdout, stderr string) []response {
	return []response{
		result(stdout, stderr, 0, nil),
		result("container-id\n", "", 0, nil),
		result("container-id\n", "", 0, nil),
		result("", "Error: No such container: container-id\n", 1, errCommand),
		result("network-id\n", "", 0, nil),
		result("", "Error: No such network: aries-net-fixedid\n", 1, errCommand),
	}
}

func testEnvironment() core.Environment {
	return core.Environment{
		Image:        "sha256:" + strings.Repeat("a", 64),
		Workdir:      "/work",
		CPU:          1.5,
		MemoryMB:     64,
		StorageMB:    32,
		AllowNetwork: false,
		Env:          map[string]string{"ZED": "last", "ALPHA": "first"},
	}
}

func testRequest() core.SandboxRequest {
	return core.SandboxRequest{RunID: "run-1", TaskID: "task-1", Environment: testEnvironment()}
}

func testManager(t *testing.T, responses ...response) (*Manager, *fakeCommandRunner) {
	t.Helper()
	fake := &fakeCommandRunner{responses: responses}
	helperPath := filepath.Join(t.TempDir(), "aries-exec-helper")
	if err := os.WriteFile(helperPath, []byte("test helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		cli:            fake,
		outputDir:      t.TempDir(),
		cleanupTimeout: 100 * time.Millisecond,
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		helperPath:     helperPath,
		newID:          func() (string, error) { return "fixedid", nil },
		engine:         &fakeEngine{},
	}
	return manager, fake
}

func TestStartAppliesExactIdentityAndResourceArguments(t *testing.T) {
	responses := append(successfulStartResponses(), successfulStopResponses("sandbox stdout", "sandbox stderr")...)
	manager, fake := testManager(t, responses...)
	live, err := manager.Start(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	sandbox := live.(*Sandbox)
	runtimeDir := sandbox.runtimeDir
	stagedHelper, err := os.Stat(filepath.Join(sandbox.artifactDir, "helper", "aries-exec-helper"))
	if err != nil || !stagedHelper.Mode().IsRegular() || stagedHelper.Mode().Perm() != 0o555 {
		t.Fatalf("staged exec helper = %v, %v", stagedHelper, err)
	}
	calls := fake.snapshot()
	for _, labels := range [][]string{
		{"--label", "aries.run=run-1"},
		{"--label", "aries.task=task-1"},
		{"--label", "aries.milestone=m3"},
	} {
		if !containsSequence(calls[0].args, labels) || !containsSequence(calls[1].args, labels) {
			t.Fatalf("identity labels %v missing from network/container calls: %#v %#v", labels, calls[0].args, calls[1].args)
		}
	}
	for _, sequence := range [][]string{
		{"--cpus", "1.5"}, {"--memory", "64m"}, {"--storage-opt", "size=32m"},
		{"--env", "ALPHA=first"}, {"--env", "ZED=last"},
		{"--entrypoint", "/bin/sleep", testEnvironment().Image, "infinity"},
	} {
		if !containsSequence(calls[1].args, sequence) {
			t.Fatalf("container create args %#v do not contain %#v", calls[1].args, sequence)
		}
	}
	createArgs := strings.Join(calls[1].args, "\n")
	for _, destination := range []string{"dst=" + helperContainerPath + ",readonly", "dst=" + socketContainerDir + ",readonly"} {
		if !strings.Contains(createArgs, destination) {
			t.Fatalf("container create args do not contain read-only mount %q: %#v", destination, calls[1].args)
		}
	}
	if slices.Contains(calls[1].args, "-c") {
		t.Fatalf("keepalive invokes a shell: %#v", calls[1].args)
	}
	if sandbox.runID != "run-1" || sandbox.taskID != "task-1" {
		t.Fatalf("sandbox identity = %q/%q", sandbox.runID, sandbox.taskID)
	}
	if err := sandbox.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if _, err := os.Lstat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private exec runtime still exists: %v", err)
	}
	if err := sandbox.Stop(context.Background()); err != nil {
		t.Fatalf("repeated Stop() error = %v", err)
	}
	if got := len(fake.snapshot()); got != len(responses) {
		t.Fatalf("Docker calls = %d, want %d", got, len(responses))
	}
	for _, name := range []string{"container.stdout.log", "container.stderr.log"} {
		info, err := os.Stat(filepath.Join(sandbox.artifactDir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("log %q = %v, %v", name, info, err)
		}
	}
}

func TestStartRejectsUnsafeIdentityBeforeDocker(t *testing.T) {
	manager, fake := testManager(t)
	for _, request := range []core.SandboxRequest{
		{RunID: "", TaskID: "task-1", Environment: testEnvironment()},
		{RunID: "run/escape", TaskID: "task-1", Environment: testEnvironment()},
		{RunID: "run-1", TaskID: "../task", Environment: testEnvironment()},
	} {
		if _, err := manager.Start(context.Background(), request); err == nil {
			t.Fatalf("Start(%#v) accepted unsafe identity", request)
		}
	}
	if len(fake.snapshot()) != 0 {
		t.Fatal("unsafe identity reached Docker")
	}
}

func TestStartRejectsUntrustedExecHelperBeforeDocker(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T) string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }},
		{"not executable", func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "helper")
			if err := os.WriteFile(path, []byte("helper"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"symlink", func(t *testing.T) string {
			directory := t.TempDir()
			target := filepath.Join(directory, "target")
			link := filepath.Join(directory, "helper")
			if err := os.WriteFile(target, []byte("helper"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, fake := testManager(t)
			manager.helperPath = test.setup(t)
			if _, err := manager.Start(context.Background(), testRequest()); err == nil || !strings.Contains(err.Error(), "exec helper") {
				t.Fatalf("Start() error = %v", err)
			}
			if len(fake.snapshot()) != 0 {
				t.Fatal("untrusted helper reached Docker")
			}
		})
	}
}

func TestStartRollsBackEveryPartialAcquisition(t *testing.T) {
	tests := []struct {
		name      string
		responses []response
		wantCalls int
	}{
		{"network", []response{
			result("", "create failed", 1, errCommand),
			result("", "No such network", 1, errCommand), result("", "No such network", 1, errCommand),
		}, 3},
		{"create", []response{
			result("network", "", 0, nil), result("", "create failed", 1, errCommand),
			result("container", "", 0, nil), result("", "No such container", 1, errCommand),
			result("network", "", 0, nil), result("", "No such network", 1, errCommand),
		}, 6},
		{"start", []response{
			result("network", "", 0, nil), result("container", "", 0, nil), result("", "start failed", 1, errCommand),
			result("container", "", 0, nil), result("", "No such container", 1, errCommand),
			result("network", "", 0, nil), result("", "No such network", 1, errCommand),
		}, 7},
		{"container inspect", []response{
			result("network", "", 0, nil), result("container", "", 0, nil), result("container", "", 0, nil), result("", "inspect failed", 1, errCommand),
			result("container", "", 0, nil), result("container", "", 0, nil), result("", "No such container", 1, errCommand),
			result("network", "", 0, nil), result("", "No such network", 1, errCommand),
		}, 9},
		{"network inspect", []response{
			result("network", "", 0, nil), result("container", "", 0, nil), result("container", "", 0, nil), result(successfulInspection(true), "", 0, nil), result("", "network inspect failed", 1, errCommand),
			result("container", "", 0, nil), result("container", "", 0, nil), result("", "No such container", 1, errCommand),
			result("network", "", 0, nil), result("", "No such network", 1, errCommand),
		}, 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, fake := testManager(t, test.responses...)
			_, err := manager.Start(context.Background(), testRequest())
			if !errors.Is(err, errCommand) {
				t.Fatalf("Start() error = %v, want primary cause", err)
			}
			if got := len(fake.snapshot()); got != test.wantCalls {
				t.Fatalf("calls = %d, want %d: %#v", got, test.wantCalls, fake.snapshot())
			}
		})
	}
}

type engineResponse struct {
	result   core.CommandResult
	launched bool
	err      error
	wait     <-chan struct{}
	entered  chan<- struct{}
}

type fakeEngine struct {
	mu        sync.Mutex
	responses []engineResponse
	commands  []core.Command
}

func (f *fakeEngine) Exec(ctx context.Context, _, _ string, command core.Command) (core.CommandResult, bool, error) {
	f.mu.Lock()
	index := len(f.commands)
	f.commands = append(f.commands, command)
	response := engineResponse{result: core.CommandResult{ExitCode: 0}}
	if index < len(f.responses) {
		response = f.responses[index]
	}
	f.mu.Unlock()
	if response.entered != nil {
		response.entered <- struct{}{}
	}
	if response.wait != nil {
		select {
		case <-response.wait:
		case <-ctx.Done():
			return core.CommandResult{ExitCode: -1}, true, ctx.Err()
		}
	}
	return response.result, response.launched, response.err
}

func (f *fakeEngine) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

func testExecSandbox(t *testing.T, engine execEngine, responses ...response) (*Sandbox, *fakeCommandRunner) {
	t.Helper()
	cli := &fakeCommandRunner{responses: responses}
	return &Sandbox{
		cli:            cli,
		containerID:    "container-id",
		networkName:    "aries-net-fixedid",
		workdir:        "/work",
		artifactDir:    t.TempDir(),
		runtimeDir:     t.TempDir(),
		outputDir:      t.TempDir(),
		cleanupTimeout: time.Second,
		runID:          "run-1",
		taskID:         "task-1",
		engine:         engine,
		execGate:       make(chan struct{}, 1),
	}, cli
}

func TestExecUsesDaemonResultAndRestartIsFailClosed(t *testing.T) {
	engine := &fakeEngine{responses: []engineResponse{
		{result: core.CommandResult{ExitCode: 7, Stdout: "out", Stderr: "err"}, launched: true},
		{result: core.CommandResult{ExitCode: -1}, launched: true, err: context.DeadlineExceeded},
		{result: core.CommandResult{ExitCode: 0, Stdout: "after"}, launched: true},
	}}
	restartResponses := []response{
		result("container", "", 0, nil),
		result(successfulInspection(false), "", 0, nil),
		result("container", "", 0, nil),
		result(successfulInspection(true), "", 0, nil),
		result(successfulNetworkInspection(), "", 0, nil),
	}
	sandbox, cli := testExecSandbox(t, engine, restartResponses...)
	first, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/false"})
	if err != nil || first.ExitCode != 7 || first.Stdout != "out" || first.Stderr != "err" {
		t.Fatalf("first Exec() = %#v, %v", first, err)
	}
	_, err = sandbox.Exec(context.Background(), core.Command{Path: "/bin/sleep", Args: []string{"10"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("failed Exec() error = %v", err)
	}
	third, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/true"})
	if err != nil || third.Stdout != "after" {
		t.Fatalf("post-restart Exec() = %#v, %v", third, err)
	}
	for _, call := range cli.snapshot() {
		joined := strings.Join(call.args, " ")
		if strings.Contains(joined, " exec ") || strings.Contains(joined, "/tmp/.aries-exec") || strings.Contains(joined, " kill ") {
			t.Fatalf("exec cleanup exposed a cross-exec kill surface: %q", joined)
		}
	}
	want := [][]string{
		{"container", "stop", "--time", "1", "container-id"},
		{"container", "inspect", "container-id"},
		{"container", "start", "container-id"},
		{"container", "inspect", "container-id"},
		{"network", "inspect", "aries-net-fixedid"},
	}
	calls := cli.snapshot()
	for index := range want {
		if !reflect.DeepEqual(calls[index].args, want[index]) {
			t.Fatalf("restart call %d = %#v, want %#v", index, calls[index].args, want[index])
		}
	}
}

func TestExecJoinsRestartFailureWithToolFailure(t *testing.T) {
	engineFailure := errors.New("exec helper transport failed")
	engine := &fakeEngine{responses: []engineResponse{{
		result:   core.CommandResult{ExitCode: -1},
		launched: true,
		err:      engineFailure,
	}}}
	sandbox, _ := testExecSandbox(t, engine,
		result("container", "", 0, nil),
		result(successfulInspection(false), "", 0, nil),
		result("", "start failed", 1, errCleanup),
	)
	_, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/true"})
	if !errors.Is(err, engineFailure) || !errors.Is(err, errCleanup) {
		t.Fatalf("Exec() error = %v, want engine and restart failures", err)
	}
}

func TestExecRestartRejectsMissingOrWritableHelperMount(t *testing.T) {
	for _, test := range []struct {
		name       string
		inspection string
	}{
		{
			name:       "socket missing",
			inspection: strings.Replace(successfulInspection(true), `,{"Destination":"/run/aries","RW":false}`, "", 1),
		},
		{
			name:       "socket writable",
			inspection: strings.Replace(successfulInspection(true), `"Destination":"/run/aries","RW":false`, `"Destination":"/run/aries","RW":true`, 1),
		},
		{
			name:       "helper missing",
			inspection: strings.Replace(successfulInspection(true), `{"Destination":"/opt/aries/bin/aries-exec-helper","RW":false},`, "", 1),
		},
		{
			name:       "helper writable",
			inspection: strings.Replace(successfulInspection(true), `"Destination":"/opt/aries/bin/aries-exec-helper","RW":false`, `"Destination":"/opt/aries/bin/aries-exec-helper","RW":true`, 1),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engineFailure := errors.New("exec helper transport failed")
			engine := &fakeEngine{responses: []engineResponse{{
				result:   core.CommandResult{ExitCode: -1},
				launched: true,
				err:      engineFailure,
			}}}
			sandbox, _ := testExecSandbox(t, engine,
				result("container", "", 0, nil),
				result(successfulInspection(false), "", 0, nil),
				result("container", "", 0, nil),
				result(test.inspection, "", 0, nil),
			)
			_, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/true"})
			if !errors.Is(err, engineFailure) || !strings.Contains(err.Error(), "trusted helper mounts") {
				t.Fatalf("Exec() error = %v", err)
			}
		})
	}
}

func TestExecSerializesConcurrentCommands(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	engine := &fakeEngine{responses: []engineResponse{{launched: true, wait: release, entered: entered}}}
	sandbox, _ := testExecSandbox(t, engine)
	firstDone := make(chan error, 1)
	go func() {
		_, err := sandbox.Exec(context.Background(), core.Command{Path: "/bin/sleep", Args: []string{"1"}})
		firstDone <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := sandbox.Exec(ctx, core.Command{Path: "/bin/true"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("concurrent Exec() error = %v, want deadline", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Exec() error = %v", err)
	}
	if engine.count() != 1 {
		t.Fatalf("engine calls = %d, want one serialized call", engine.count())
	}
}

func TestUploadUsesPrivateStageInsteadOfCallerPath(t *testing.T) {
	artifactDir := t.TempDir()
	outputDir := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	var staged []byte
	cli := &fakeCommandRunner{responses: []response{func(_ context.Context, call recordedCall) (commandResult, error) {
		if call.args[2] == source {
			return commandResult{}, errors.New("Docker cp consumed caller-controlled source path")
		}
		var err error
		staged, err = os.ReadFile(call.args[2])
		return commandResult{exitCode: 0}, err
	}}}
	sandbox := &Sandbox{cli: cli, containerID: "container", artifactDir: artifactDir, outputDir: outputDir}
	if err := sandbox.Upload(context.Background(), source, "/tests/test.sh"); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	if string(staged) != "trusted" {
		t.Fatalf("staged bytes = %q, want trusted", staged)
	}
	if len(cli.snapshot()) != 1 {
		t.Fatalf("Docker calls = %d, want one", len(cli.snapshot()))
	}
}

func TestUploadRejectsPathSwapThatChangesOpenedMetadata(t *testing.T) {
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "source")
	backup := filepath.Join(sourceDir, "original")
	attacker := filepath.Join(sourceDir, "attacker")
	if err := os.WriteFile(source, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attacker, []byte("attacker"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &fakeCommandRunner{}
	sandbox := &Sandbox{cli: cli, containerID: "container", artifactDir: t.TempDir(), outputDir: t.TempDir()}
	sandbox.testHooks.afterUploadOpen = func() {
		if err := os.Rename(source, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(backup, 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(attacker, source); err != nil {
			t.Fatal(err)
		}
	}
	if err := sandbox.Upload(context.Background(), source, "/tests/test.sh"); err == nil || !strings.Contains(err.Error(), "changed while being staged") {
		t.Fatalf("Upload() error = %v", err)
	}
	if len(cli.snapshot()) != 0 {
		t.Fatal("Docker cp ran after opened source metadata changed")
	}
	content, err := os.ReadFile(source)
	if err != nil || string(content) != "attacker" {
		t.Fatalf("replacement path content = %q, %v", content, err)
	}
}

func TestUploadRejectsOpenedFileMutation(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("trusted"), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := &fakeCommandRunner{}
	sandbox := &Sandbox{cli: cli, containerID: "container", artifactDir: t.TempDir(), outputDir: t.TempDir()}
	sandbox.testHooks.afterUploadOpen = func() {
		if err := os.WriteFile(source, []byte("attacker"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := sandbox.Upload(context.Background(), source, "/tests/test.sh"); err == nil || !strings.Contains(err.Error(), "changed while being staged") {
		t.Fatalf("Upload() error = %v", err)
	}
	if len(cli.snapshot()) != 0 {
		t.Fatal("mutated source reached Docker cp")
	}
}

func TestDownloadPublishesPrivateRegularFile(t *testing.T) {
	outputDir := t.TempDir()
	artifactDir := filepath.Join(outputDir, "sandboxes", "id")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 1, 2, 255}
	cli := &fakeCommandRunner{responses: []response{func(_ context.Context, call recordedCall) (commandResult, error) {
		return commandResult{exitCode: 0}, os.WriteFile(call.args[3], want, 0o640)
	}}}
	sandbox := &Sandbox{cli: cli, containerID: "container", artifactDir: artifactDir, outputDir: outputDir}
	destination := filepath.Join(outputDir, "evaluation", "reward.bin")
	if err := sandbox.Download(context.Background(), "/logs/reward.bin", destination); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("downloaded bytes = %v, %v", got, err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("downloaded mode = %v, %v", info, err)
	}
}

func TestDownloadRejectsParentAndFinalSymlinkSwaps(t *testing.T) {
	for _, finalSymlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "parent", true: "final"}[finalSymlink], func(t *testing.T) {
			outputDir := t.TempDir()
			artifactDir := filepath.Join(outputDir, "sandboxes", "id")
			if err := os.MkdirAll(artifactDir, 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			destination := filepath.Join(outputDir, "evaluation", "reward.txt")
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				t.Fatal(err)
			}
			if finalSymlink {
				if err := os.Symlink(filepath.Join(outside, "escaped"), destination); err != nil {
					t.Fatal(err)
				}
			}
			cli := &fakeCommandRunner{responses: []response{func(_ context.Context, call recordedCall) (commandResult, error) {
				return commandResult{exitCode: 0}, os.WriteFile(call.args[3], []byte("1\n"), 0o600)
			}}}
			sandbox := &Sandbox{cli: cli, containerID: "container", artifactDir: artifactDir, outputDir: outputDir}
			if !finalSymlink {
				sandbox.testHooks.beforeDownloadWalk = func() {
					parent := filepath.Dir(destination)
					if err := os.Rename(parent, parent+"-real"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, parent); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := sandbox.Download(context.Background(), "/logs/reward.txt", destination); err == nil {
				t.Fatal("Download() accepted a hostile symlink swap")
			}
			if _, err := os.Stat(filepath.Join(outside, "reward.txt")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("parent swap escaped output root: %v", err)
			}
			if _, err := os.Stat(filepath.Join(outside, "escaped")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("final symlink was followed: %v", err)
			}
		})
	}
}

func TestStopIsConcurrentAndIdempotent(t *testing.T) {
	responses := append(successfulStartResponses(), successfulStopResponses("", "")...)
	manager, fake := testManager(t, responses...)
	live, err := manager.Start(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*Sandbox)
	const callers = 8
	results := make(chan error, callers)
	for range callers {
		go func() { results <- sandbox.Stop(context.Background()) }()
	}
	for range callers {
		if err := <-results; err != nil {
			t.Fatalf("concurrent Stop() error = %v", err)
		}
	}
	if err := sandbox.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.snapshot()); got != len(responses) {
		t.Fatalf("calls = %d, want %d", got, len(responses))
	}
}

func containsSequence(values, sequence []string) bool {
	for index := 0; index+len(sequence) <= len(values); index++ {
		if slices.Equal(values[index:index+len(sequence)], sequence) {
			return true
		}
	}
	return false
}
