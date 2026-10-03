package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

type fakeDeployment struct {
	deployment.Deployment
	request                                      deployment.Request
	environmentRequest                           core.SandboxRequest
	calls                                        []string
	createErr, validateErr, stopErr, downloadErr error
	logsErr                                      error
	uploadErr                                    error
	downloadReader                               io.ReadCloser
	closeCalls                                   int
	stopCalls, networkStops                      int
	cleanupCanceled                              bool
	command                                      core.Command
	input, upload                                []byte
	uploadDir                                    string
	archive                                      []byte
	info                                         deployment.FileInfo
}

type fakeEnvironment struct {
	f                *fakeDeployment
	startErr         error
	validationErr    error
	validations      int
	failValidationAt int
}

func (e *fakeEnvironment) Start(_ context.Context, r core.SandboxRequest) (string, error) {
	e.f.environmentRequest = r
	return "private-network", e.startErr
}
func (e *fakeEnvironment) Validate(context.Context) error {
	e.validations++
	if e.validations == e.failValidationAt {
		return e.validationErr
	}
	return nil
}
func (e *fakeEnvironment) Stop(ctx context.Context) error {
	e.f.networkStops++
	e.f.cleanupCanceled = ctx.Err() != nil
	return nil
}
func (e *fakeEnvironment) BridgeListen(context.Context) (core.BridgeListen, error) {
	return core.BridgeListen{BindHost: "10.0.0.1", AdvertiseHost: "10.0.0.1"}, nil
}
func (f *fakeDeployment) Create(_ context.Context, r deployment.Request) (string, error) {
	f.request = r
	f.calls = append(f.calls, "create")
	return "container-id", f.createErr
}
func (f *fakeDeployment) Validate(context.Context, string, deployment.Request, [][]byte) error {
	f.calls = append(f.calls, "validate")
	return f.validateErr
}
func (f *fakeDeployment) Start(context.Context, string) error {
	f.calls = append(f.calls, "start")
	return nil
}
func (f *fakeDeployment) Running(context.Context, string) (bool, error) { return true, nil }
func (f *fakeDeployment) Stop(ctx context.Context, _ string) error {
	f.stopCalls++
	f.cleanupCanceled = ctx.Err() != nil
	return f.stopErr
}
func (f *fakeDeployment) LogsStream(_ context.Context, _ string, out, errout io.Writer) error {
	_, err := io.WriteString(out, "out")
	return errors.Join(err, f.logsErr)
}
func (f *fakeDeployment) ExecStream(_ context.Context, _ string, c core.Command, in io.Reader, out, errout io.Writer) (core.CommandResult, error) {
	f.command = c
	if in != nil {
		f.input, _ = io.ReadAll(in)
	}
	if out != nil {
		_, _ = io.WriteString(out, "stdout")
	}
	if errout != nil {
		_, _ = io.WriteString(errout, "stderr")
	}
	return core.CommandResult{ExitCode: 7}, nil
}
func (f *fakeDeployment) UploadArchive(_ context.Context, _ string, dest string, in io.Reader) error {
	f.uploadDir = dest
	if f.uploadErr != nil {
		return f.uploadErr
	}
	f.upload, _ = io.ReadAll(in)
	return nil
}
func (f *fakeDeployment) DownloadArchive(context.Context, string, string) (io.ReadCloser, deployment.FileInfo, error) {
	if f.downloadReader != nil {
		return f.downloadReader, f.info, f.downloadErr
	}
	return io.NopCloser(bytes.NewReader(f.archive)), f.info, f.downloadErr
}
func testRequest() core.SandboxRequest {
	return core.SandboxRequest{RunID: "run", TaskID: "task", Environment: core.Environment{Image: "busybox:1.37.0", Workdir: "/work", ExecUser: "1000:1000", CPU: 0.5, MemoryMB: 32, StorageMB: 64, GPUs: 1}}
}
func testManager(t *testing.T, f *fakeDeployment) *Manager {
	t.Helper()
	m, err := New(Options{Deployment: f, NewEnvironment: func() deployment.TaskEnvironment { return &fakeEnvironment{f: f} }, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func startSandbox(t *testing.T, f *fakeDeployment) *Sandbox {
	t.Helper()
	m := testManager(t, f)
	s, err := m.Start(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	return s.(*Sandbox)
}

func TestSharedDeploymentReceivesSandboxPolicy(t *testing.T) {
	t.Setenv("TZ", "Europe/Paris")
	f := &fakeDeployment{}
	request := testRequest()
	request.Environment.Env = map[string]string{"TZ": "untrusted", "DEBIAN_FRONTEND": "interactive", "KEEP": "exact"}
	m := testManager(t, f)
	live, err := m.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	s := live.(*Sandbox)
	r := f.request
	if r.Workdir != "/work" || !r.Init || !r.NoNewPrivileges || !r.AllowImageVolumes || r.StorageMB != 64 || r.GPUs != 1 || *r.CPU != 0.5 || *r.MemoryMB != 32 {
		t.Fatalf("lost policy: %+v", r)
	}
	if !reflect.DeepEqual(r.Entrypoint, []string{"/bin/sleep"}) || !reflect.DeepEqual(r.Args, []string{"infinity"}) || !reflect.DeepEqual(r.NetworkAliases, []string{networkAlias}) {
		t.Fatalf("runtime: %+v", r)
	}
	if r.Network != s.NetworkName() || f.environmentRequest.Environment.AllowNetwork || r.Labels["aries.component"] != "sandbox" || f.environmentRequest.RunID != "run" {
		t.Fatalf("ownership: %+v %+v", r, f.environmentRequest)
	}
	if !slices.Contains(r.Env, "TZ=Europe/Paris") || !slices.Contains(r.Env, "DEBIAN_FRONTEND=noninteractive") || !slices.Contains(r.Env, "KEEP=exact") {
		t.Fatal(r.Env)
	}
	if gateway, err := s.BridgeListen(context.Background()); err != nil || gateway.AdvertiseHost != "10.0.0.1" {
		t.Fatal(gateway, err)
	}
}
func TestPartialCreationAndValidationFailureRollbackWithFreshContext(t *testing.T) {
	for _, validation := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "validate"}[validation], func(t *testing.T) {
			failure := errors.New("failed")
			f := &fakeDeployment{}
			if validation {
				f.validateErr = failure
			} else {
				f.createErr = failure
			}
			m := testManager(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := m.Start(ctx, testRequest()); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if f.stopCalls != 1 || f.networkStops != 1 || f.cleanupCanceled {
				t.Fatalf("cleanup: %+v", f)
			}
		})
	}
}
func TestStopConcurrentRetryAndOwnership(t *testing.T) {
	f := &fakeDeployment{}
	s := startSandbox(t, f)
	foreign := testManager(t, &fakeDeployment{})
	if foreign.Stop(context.Background(), s) == nil || s.owner.Stop(context.Background(), nil) == nil {
		t.Fatal("accepted foreign/nil sandbox")
	}
	f.stopErr = errors.New("absence unconfirmed")
	if s.owner.Stop(context.Background(), s) == nil {
		t.Fatal("lost stop failure")
	}
	if !s.containerOwned || !s.networkOwned || f.networkStops != 0 {
		t.Fatal("lost ownership")
	}
	f.stopErr = nil
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.owner.Stop(context.Background(), s); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.stopCalls != 2 || f.networkStops != 1 {
		t.Fatal(f.stopCalls, f.networkStops)
	}
	for _, name := range []string{"container.stdout.log", "container.stderr.log"} {
		info, err := os.Stat(filepath.Join(s.artifactDir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(info, err)
		}
	}
}
func TestExecPreservesDefaultsOverridesAndStreaming(t *testing.T) {
	f := &fakeDeployment{}
	s := startSandbox(t, f)
	result, err := s.Exec(context.Background(), core.Command{Path: "/bin/cat", Stdin: []byte("input"), Args: []string{"a b", "$literal"}})
	if err != nil || result.ExitCode != 7 || result.Stdout != "stdout" || result.Stderr != "stderr" || string(f.input) != "input" || f.command.Dir != "/work" || f.command.User != "1000:1000" {
		t.Fatalf("%+v %+v %v", result, f.command, err)
	}
	_, err = s.ExecStream(context.Background(), core.Command{Path: "/bin/cat", Dir: "/", User: "0:0"}, strings.NewReader("stream"), io.Discard, io.Discard)
	if err != nil || f.command.Dir != "/" || f.command.User != "0:0" || string(f.input) != "stream" {
		t.Fatal(f.command, err)
	}
}
func archive(t *testing.T, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "file", Mode: 0644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(tw, content)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestTransfersKeepModesBoundsAndAtomicPublication(t *testing.T) {
	f := &fakeDeployment{archive: archive(t, "content"), info: deployment.FileInfo{Size: 7, Mode: 0644}}
	s := startSandbox(t, f)
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte("upload"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := s.Upload(context.Background(), src, "/work/upload"); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(f.upload))
	header, err := tr.Next()
	if err != nil || header.Name != "upload" || header.Mode != 0750 || f.uploadDir != "/work" {
		t.Fatal(header, err)
	}
	dest := filepath.Join(s.outputDir, "file")
	if err := os.WriteFile(dest, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{7, 1} {
		f.info.Size = size
		if err := s.DownloadLimit(context.Background(), "/work/file", dest, 3); err == nil {
			t.Fatal("accepted oversized archive")
		}
		got, _ := os.ReadFile(dest)
		if string(got) != "previous" {
			t.Fatal("modified destination")
		}
	}
	f.info.Size = 7
	if err := s.DownloadLimit(context.Background(), "/work/file", dest, 7); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	info, _ := os.Stat(dest)
	if string(got) != "content" || info.Mode().Perm() != 0600 {
		t.Fatal(string(got), info)
	}
	if err := s.Download(context.Background(), "/work/file", filepath.Join(t.TempDir(), "outside")); err == nil {
		t.Fatal("accepted outside destination")
	}
	f.archive = f.archive[:515]
	if err := s.Download(context.Background(), "/work/file", dest); err == nil {
		t.Fatal("accepted truncated archive")
	}
	got, _ = os.ReadFile(dest)
	if string(got) != "content" {
		t.Fatal("failed copy replaced destination")
	}
}
func TestDownloadKeepsMissingFileDistinctFromLostRuntime(t *testing.T) {
	f := &fakeDeployment{}
	s := startSandbox(t, f)
	for _, failure := range []error{runner.ErrNotFound, errors.New("container lost")} {
		f.downloadErr = failure
		err := s.Download(context.Background(), "/work/file", filepath.Join(s.outputDir, "file"))
		if !errors.Is(err, failure) || errors.Is(err, runner.ErrNotFound) != (failure == runner.ErrNotFound) {
			t.Fatal(err)
		}
	}
}
func TestValidationBeforeDeployment(t *testing.T) {
	for _, change := range []func(*core.SandboxRequest){func(r *core.SandboxRequest) { r.TaskID = "../bad" }, func(r *core.SandboxRequest) { r.Environment.Workdir = "relative" }, func(r *core.SandboxRequest) { r.Environment.ExecUser = "root" }, func(r *core.SandboxRequest) { r.Environment.Image = "busybox" }, func(r *core.SandboxRequest) { r.Environment.CPU = -1 }} {
		f := &fakeDeployment{}
		m := testManager(t, f)
		r := testRequest()
		change(&r)
		if _, err := m.Start(context.Background(), r); err == nil || len(f.calls) != 0 {
			t.Fatal(err, f.calls)
		}
	}
	if _, err := New(Options{OutputDir: t.TempDir()}); err == nil {
		t.Fatal("accepted missing deployment")
	}
}

func TestZeroResourcesPreserveUnlimitedSandboxDefaults(t *testing.T) {
	f := &fakeDeployment{}
	m := testManager(t, f)
	request := testRequest()
	request.Environment.CPU = 0
	request.Environment.MemoryMB = 0
	request.Environment.ExecUser = ""
	request.Environment.AllowNetwork = true
	if _, err := m.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if f.request.CPU != nil || f.request.MemoryMB != nil || f.request.NoNewPrivileges || !f.environmentRequest.Environment.AllowNetwork {
		t.Fatalf("defaults changed: %+v %+v", f.request, f.environmentRequest)
	}
}

func TestValidateEnvironmentRejectsResourceConversionOverflow(t *testing.T) {
	environment := testRequest().Environment
	environment.CPU = math.Exp2(63) / 1e9
	if err := validateEnvironment(environment); err == nil {
		t.Fatal("accepted overflowing CPU")
	}
	environment = testRequest().Environment
	environment.MemoryMB = int(math.MaxInt64>>20) + 1
	if err := validateEnvironment(environment); err == nil {
		t.Fatal("accepted overflowing memory")
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

func TestExecUserValidationRejectsNamesAndMalformedIDs(t *testing.T) {
	for _, user := range []string{"root", "1000", "1000:", ":1000", "1:2:3", "-1:0", "1.0:2", " 1:2", "1:2 "} {
		t.Run(user, func(t *testing.T) {
			environment := testRequest().Environment
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
		environment := testRequest().Environment
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

func TestCommandOutputLimitValidation(t *testing.T) {
	for _, limit := range []int{-1, maxConfiguredOutput + 1} {
		if err := validateCommand(core.Command{Path: "/bin/true", OutputLimitBytes: limit}); err == nil {
			t.Fatalf("validateCommand accepted output limit %d", limit)
		}
	}
}

func TestValidateEnvironmentAcceptsExplicitTaskTag(t *testing.T) {
	environment := testRequest().Environment
	environment.Image = "registry.example:5000/org/task:20251031"
	if err := validateEnvironment(environment); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeDeployment) Close() error { f.closeCalls++; return nil }

func TestAlreadyAbsentRuntimeDoesNotPreventPositiveCleanup(t *testing.T) {
	for _, logErr := range []error{runner.ErrNotFound, errors.New("logs unavailable")} {
		f := &fakeDeployment{logsErr: logErr}
		s := startSandbox(t, f)
		err := s.owner.Stop(context.Background(), s)
		if errors.Is(logErr, runner.ErrNotFound) {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, logErr) {
			t.Fatal(err)
		}
		if f.stopCalls != 1 || f.networkStops != 1 || !s.stopped {
			t.Fatal("absence was not confirmed")
		}
		if err := s.owner.Stop(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManagerCloseOwnsOnlyItsInjectedDeployment(t *testing.T) {
	f := &fakeDeployment{}
	m := testManager(t, f)
	for range 2 {
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if f.closeCalls != 1 {
		t.Fatal(f.closeCalls)
	}
}

type observedArchive struct {
	reads  int
	closed bool
}

func (r *observedArchive) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }
func (r *observedArchive) Close() error             { r.closed = true; return nil }

func TestOversizeStatRejectsBeforeReadingArchive(t *testing.T) {
	reader := &observedArchive{}
	f := &fakeDeployment{downloadReader: reader, info: deployment.FileInfo{Size: 100, Mode: 0600}}
	s := startSandbox(t, f)
	err := s.DownloadLimit(context.Background(), "/work/file", filepath.Join(s.outputDir, "file"), 10)
	if err == nil || reader.reads != 0 || !reader.closed {
		t.Fatalf("err=%v reads=%d closed=%v", err, reader.reads, reader.closed)
	}
}

func TestUploadUnblocksArchiveProducerWhenDeploymentRejectsStream(t *testing.T) {
	failure := errors.New("upload rejected")
	f := &fakeDeployment{uploadErr: failure}
	s := startSandbox(t, f)
	source := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(source, bytes.Repeat([]byte("x"), 1<<20), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Upload(context.Background(), source, "/work/file") }()
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("archive producer did not unblock after deployment rejection")
	}
}

func TestEnvironmentFailuresRollbackAndBlockRuntimeExposure(t *testing.T) {
	for _, stage := range []string{"start", "before-container", "after-container"} {
		t.Run(stage, func(t *testing.T) {
			f := &fakeDeployment{}
			failure := errors.New("environment ownership uncertain")
			e := &fakeEnvironment{f: f, validationErr: failure}
			switch stage {
			case "start":
				e.startErr = failure
			case "before-container":
				e.failValidationAt = 1
			case "after-container":
				e.failValidationAt = 2
			}
			m := testManager(t, f)
			m.newEnvironment = func() deployment.TaskEnvironment { return e }
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := m.Start(ctx, testRequest()); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if f.networkStops != 1 || f.cleanupCanceled {
				t.Fatal("environment rollback did not use fresh context")
			}
			if stage == "after-container" {
				if f.stopCalls != 1 {
					t.Fatal("runtime not removed")
				}
			} else if f.stopCalls != 0 || len(f.calls) != 0 {
				t.Fatal("runtime allocated before environment validated")
			}
			if _, err := m.BridgeListen(context.Background()); err == nil {
				t.Fatal("failed startup became bridge-visible")
			}
		})
	}
}

func TestBridgeResolutionRejectsAmbiguousOccurrences(t *testing.T) {
	f := &fakeDeployment{}
	m := testManager(t, f)
	ctx := context.Background()
	if _, err := m.BridgeListen(ctx); err == nil {
		t.Fatal("resolved without occurrence")
	}
	first, err := m.Start(ctx, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.BridgeListen(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := m.Start(ctx, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.BridgeListen(ctx); err == nil {
		t.Fatal("selected arbitrary occurrence")
	}
	if err := m.Stop(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BridgeListen(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BridgeListen(ctx); err == nil {
		t.Fatal("resolved after cleanup")
	}
}

func TestCloseRetriesFailedStartupCleanupBeforeClosingTransport(t *testing.T) {
	failure := errors.New("container removal uncertain")
	f := &fakeDeployment{createErr: errors.New("partial create"), stopErr: failure}
	m := testManager(t, f)
	if _, err := m.Start(context.Background(), testRequest()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := m.Close(); !errors.Is(err, failure) || f.closeCalls != 0 || f.networkStops != 0 {
		t.Fatal("closed transport before positive cleanup", err)
	}
	f.stopErr = nil
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if f.networkStops != 1 || f.closeCalls != 1 {
		t.Fatal("partial startup not reclaimed", f.networkStops, f.closeCalls)
	}
	if err := m.Close(); err != nil || f.closeCalls != 1 {
		t.Fatal(err, f.closeCalls)
	}
}

func TestPathAndDownloadLimitRejectionBeforeDeployment(t *testing.T) {
	// A nil deployment turns any accidental external call into a test failure.
	s := &Sandbox{outputDir: t.TempDir()}
	ctx := context.Background()
	for _, path := range []string{"/", "relative", "/unclean/..", "/bad\x00path"} {
		if err := s.Upload(ctx, "unused", path); err == nil {
			t.Fatalf("accepted upload destination %q", path)
		}
		if err := s.Download(ctx, path, filepath.Join(s.outputDir, "file")); err == nil {
			t.Fatalf("accepted download source %q", path)
		}
	}
	if err := s.DownloadLimit(ctx, "/file", filepath.Join(s.outputDir, "file"), -1); err == nil {
		t.Fatal("accepted negative byte limit")
	}
	for _, dir := range []string{"relative", "/unclean/..", "/bad\x00dir"} {
		if _, err := s.Exec(ctx, core.Command{Path: "/bin/true", Dir: dir}); err == nil {
			t.Fatalf("accepted workdir %q", dir)
		}
	}
	if _, err := s.Exec(ctx, core.Command{Path: "/"}); err == nil {
		t.Fatal("accepted root executable")
	}
	f := &fakeDeployment{}
	m := testManager(t, f)
	request := testRequest()
	request.Environment.Workdir = "/"
	if _, err := m.Start(ctx, request); err != nil || f.request.Workdir != "/" {
		t.Fatal("rejected root workdir", err)
	}
}
