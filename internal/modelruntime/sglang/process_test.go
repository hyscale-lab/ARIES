package sglang

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func helper(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "python helper")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSGLangProcessPreservesArgvAndStopsProcessGroup(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	executable := helper(t, `printf '%s\n' "$@" > "$ARGS_OUT"; sleep 60 & wait`)
	t.Setenv("ARGS_OUT", args)
	configPath := filepath.Join(t.TempDir(), "native config.yaml")
	runtime, err := New(Options{Executable: executable, ConfigPath: configPath, OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), forcedProcessWait)
		defer cancel()
		if err := runtime.Stop(ctx); err != nil {
			t.Errorf("cleanup Stop=%v", err)
		}
	})
	want := "-m\nsglang.launch_server\n--config\n" + configPath + "\n"
	deadline := time.Now().Add(time.Second)
	for {
		content, err := os.ReadFile(args)
		if err == nil && string(content) == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not record complete argv: content=%q err=%v", content, err)
		}
		time.Sleep(time.Millisecond)
	}
	content, _ := os.ReadFile(args)
	if string(content) != want {
		t.Fatalf("argv=%q want=%q", content, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if runtime.Err() != nil {
		t.Fatalf("intentional stop err=%v", runtime.Err())
	}
}

func TestStartCreatesPrivateExclusiveArtifactsAndRejectsSecondStart(t *testing.T) {
	output := t.TempDir()
	runtime, err := New(Options{Executable: helper(t, "sleep 60"), ConfigPath: "config", OutputDir: output, BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("second Start=%v", err)
	}
	artifactDir := filepath.Join(output, "sglang")
	assertMode(t, artifactDir, 0o700)
	assertMode(t, filepath.Join(artifactDir, "stdout.log"), 0o600)
	assertMode(t, filepath.Join(artifactDir, "stderr.log"), 0o600)
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	second, err := New(Options{Executable: helper(t, "exit 0"), ConfigPath: "config", OutputDir: output, BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Start(context.Background()); err == nil {
		t.Fatal("exclusive artifact collision was accepted")
	}
}

func TestStartFailureClosesPartialArtifactsAndCanceledStartHasNoEffects(t *testing.T) {
	output := t.TempDir()
	canceled, err := New(Options{Executable: helper(t, "exit 0"), ConfigPath: "config", OutputDir: output, BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := canceled.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Start=%v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "sglang")); !os.IsNotExist(err) {
		t.Fatalf("canceled Start artifact=%v", err)
	}

	invalid := filepath.Join(t.TempDir(), "invalid executable")
	if err := os.WriteFile(invalid, []byte("not an executable format"), 0o700); err != nil {
		t.Fatal(err)
	}
	failed, err := New(Options{Executable: invalid, ConfigPath: "config", OutputDir: output, BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := failed.Start(context.Background()); err == nil {
		t.Fatal("invalid executable started")
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		path := filepath.Join(output, "sglang", name)
		if countOpenFileDescriptors(path) != 0 {
			t.Fatalf("%s remained open after failed Start", name)
		}
	}
	if err := failed.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after failed Start=%v", err)
	}
}

func TestIntentionalStopIsNotUnexpectedExit(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "sleep 60"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	<-runtime.Done()
	if runtime.Err() != nil {
		t.Fatalf("Err=%v", runtime.Err())
	}
}

func TestStopRetryNeverSignalsAfterDone(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "sleep 60"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	signals := 0
	originalSignal := runtime.signalGroup
	runtime.signalGroup = func(pid int, signal syscall.Signal) error { signals++; return originalSignal(pid, signal) }
	runtime.probeGroup = func(int) error { return os.ErrPermission }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Stop(ctx); err == nil {
		t.Fatal("expected unconfirmed absence")
	}
	afterDoneSignals := signals
	runtime.probeGroup = func(int) error { return syscall.ESRCH }
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop=%v", err)
	}
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("stable Stop=%v", err)
	}
	if signals != afterDoneSignals {
		t.Fatalf("retry signaled after Done: before=%d after=%d", afterDoneSignals, signals)
	}
}

func TestCapturedStopAttemptCoalescesOwnerResult(t *testing.T) {
	done := make(chan struct{})
	close(done)
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	runtime := &Runtime{
		command: &exec.Cmd{Process: &os.Process{Pid: 12345}},
		done:    done,
		probeGroup: func(int) error {
			close(probeStarted)
			<-releaseProbe
			return os.ErrPermission
		},
	}
	ownerResult := make(chan error, 1)
	go func() { ownerResult <- runtime.Stop(context.Background()) }()
	<-probeStarted
	runtime.mu.Lock()
	attempt := runtime.stopAttempt
	runtime.mu.Unlock()
	if attempt == nil {
		t.Fatal("owner did not publish its stop attempt")
	}
	close(releaseProbe)
	ownerErr := <-ownerResult
	if ownerErr == nil {
		t.Fatal("owner unexpectedly confirmed absence")
	}
	if joinedErr := attempt.wait(context.Background()); joinedErr != ownerErr {
		t.Fatalf("coalesced result=%v owner=%v", joinedErr, ownerErr)
	}
}

func TestCompletedStopAttemptKeepsResultAfterSuccessfulRetry(t *testing.T) {
	done := make(chan struct{})
	close(done)
	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	runtime := &Runtime{
		command: &exec.Cmd{Process: &os.Process{Pid: 12345}},
		done:    done,
		probeGroup: func(int) error {
			close(probeStarted)
			<-releaseProbe
			return os.ErrPermission
		},
	}
	ownerResult := make(chan error, 1)
	go func() { ownerResult <- runtime.Stop(context.Background()) }()
	<-probeStarted
	runtime.mu.Lock()
	firstAttempt := runtime.stopAttempt
	runtime.mu.Unlock()
	if firstAttempt == nil {
		t.Fatal("owner did not publish its stop attempt")
	}
	close(releaseProbe)
	firstErr := <-ownerResult
	if firstErr == nil {
		t.Fatal("first attempt unexpectedly confirmed absence")
	}
	runtime.probeGroup = func(int) error { return syscall.ESRCH }
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop=%v", err)
	}
	if got := firstAttempt.wait(context.Background()); got != firstErr {
		t.Fatalf("completed first attempt=%v want=%v", got, firstErr)
	}
}

func TestCompletedStopAttemptWinsOverCanceledJoiningContext(t *testing.T) {
	want := errors.New("completed")
	attempt := &stopAttempt{done: make(chan struct{}), err: want}
	close(attempt.done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := attempt.wait(ctx); got != want {
		t.Fatalf("wait=%v want=%v", got, want)
	}
}

func TestCanceledStopEscalatesAndConfirmsAbsence(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "trap '' TERM; while :; do sleep 60; done"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	originalSignal := runtime.signalGroup
	var signals []syscall.Signal
	runtime.signalGroup = func(pid int, signal syscall.Signal) error {
		signals = append(signals, signal)
		return originalSignal(pid, signal)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals=%v", signals)
	}
}

func TestUnexpectedExitPublishesErrorBeforeDone(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "exit 0"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-runtime.Done()
	if runtime.Err() == nil {
		t.Fatal("expected unexpected exit")
	}
	stopErr := runtime.Stop(context.Background())
	if stopErr == nil || !strings.Contains(stopErr.Error(), "unexpectedly") {
		t.Fatalf("Stop=%v", stopErr)
	}
	var classified interface{ CleanupFailed() bool }
	if !errors.As(stopErr, &classified) || classified.CleanupFailed() {
		t.Fatalf("Stop classification=%T cleanup_failed=%t", stopErr, classified != nil && classified.CleanupFailed())
	}
	if !errors.Is(stopErr, runtime.Err()) {
		t.Fatalf("Stop=%v does not unwrap runtime Err=%v", stopErr, runtime.Err())
	}
	if retryErr := runtime.Stop(context.Background()); retryErr != stopErr {
		t.Fatalf("cached Stop=%v want identical %v", retryErr, stopErr)
	}
}

func TestExitObserverKillsResidualGroupBeforeReapAndStopDoesNotSignalObservedProcess(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "exit 0"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	originalWait := runtime.waitCommand
	reapStarted := make(chan struct{})
	releaseReap := make(chan struct{})
	runtime.waitCommand = func(command *exec.Cmd) error {
		close(reapStarted)
		<-releaseReap
		return originalWait(command)
	}
	signals := 0
	runtime.signalGroup = func(int, syscall.Signal) error {
		signals++
		return nil
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-reapStarted
	runtime.mu.Lock()
	observed := runtime.exitObserved
	done := isClosed(runtime.done)
	pid := runtime.command.Process.Pid
	runtime.mu.Unlock()
	if !observed || done {
		t.Fatalf("observed=%t done=%t", observed, done)
	}
	signaled, err := runtime.signalOwned(pid, syscall.SIGTERM)
	if err != nil || signaled || signals != 1 {
		t.Fatalf("signalOwned signaled=%t signals=%d err=%v", signaled, signals, err)
	}
	result := make(chan error, 1)
	go func() { result <- runtime.Stop(context.Background()) }()
	close(releaseReap)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "unexpectedly") {
		t.Fatalf("Stop=%v", err)
	}
	if signals != 1 {
		t.Fatalf("signals after Stop=%d", signals)
	}
}

func TestLeaderExitKillsTermIgnoringDescendantBeforeReap(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	executable := helper(t, `trap 'exit 0' TERM; sh -c 'trap "" TERM; touch "$READY"; while :; do sleep 60; done' & wait`)
	t.Setenv("READY", ready)
	runtime, err := New(Options{Executable: executable, ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	originalSignal := runtime.signalGroup
	var signals []syscall.Signal
	runtime.signalGroup = func(pid int, signal syscall.Signal) error {
		signals = append(signals, signal)
		return originalSignal(pid, signal)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runtime.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if len(signals) != 2 || signals[0] != syscall.SIGTERM || signals[1] != syscall.SIGKILL {
		t.Fatalf("signals=%v", signals)
	}
}

func TestFinalResidualKillErrorIsSurfaced(t *testing.T) {
	runtime, err := New(Options{Executable: helper(t, "exit 0"), ConfigPath: "config", OutputDir: t.TempDir(), BaseURL: "http://127.0.0.1:30000/v1"})
	if err != nil {
		t.Fatal(err)
	}
	runtime.signalGroup = func(int, syscall.Signal) error { return os.ErrPermission }
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-runtime.Done()
	err = runtime.Stop(context.Background())
	if err == nil || !strings.Contains(err.Error(), "kill residual managed SGLang process group") {
		t.Fatalf("Stop=%v", err)
	}
}

func TestStopJoinsLifecycleErrorsWhenAbsenceProtocolFails(t *testing.T) {
	done := make(chan struct{})
	close(done)
	runtime := &Runtime{
		command:         &exec.Cmd{Process: &os.Process{Pid: 12345}},
		done:            done,
		exitObserved:    true,
		residualKillErr: errors.New("residual"),
		closeErr:        errors.New("close"),
		unexpectedErr:   errors.New("unexpected"),
		probeGroup:      func(int) error { return os.ErrPermission },
	}
	err := runtime.Stop(context.Background())
	var classified interface{ CleanupFailed() bool }
	if !errors.As(err, &classified) || !classified.CleanupFailed() {
		t.Fatalf("Stop classification=%T cleanup_failed=%t", err, classified != nil && classified.CleanupFailed())
	}
	for _, want := range []string{"probe managed SGLang process group", "residual", "close", "unexpected"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Stop=%v missing %q", err, want)
		}
	}
}

func TestStopErrorClassificationAndUnwrap(t *testing.T) {
	for _, tc := range []struct {
		name          string
		protocol      error
		residual      error
		close         error
		unexpected    error
		cleanupFailed bool
	}{
		{name: "unexpected only", unexpected: errors.New("unexpected")},
		{name: "protocol", protocol: errors.New("protocol"), unexpected: errors.New("unexpected"), cleanupFailed: true},
		{name: "residual kill", residual: errors.New("residual"), cleanupFailed: true},
		{name: "log close", close: errors.New("close"), cleanupFailed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := joinStopErrors(tc.protocol, tc.residual, tc.close, tc.unexpected)
			var classified interface{ CleanupFailed() bool }
			if !errors.As(err, &classified) || classified.CleanupFailed() != tc.cleanupFailed {
				t.Fatalf("error=%T cleanup_failed=%t", err, classified != nil && classified.CleanupFailed())
			}
			for _, cause := range []error{tc.protocol, tc.residual, tc.close, tc.unexpected} {
				if cause != nil && !errors.Is(err, cause) {
					t.Fatalf("error=%v does not unwrap %v", err, cause)
				}
			}
		})
	}
}

func TestCanceledCoalescedStopWaiterDetaches(t *testing.T) {
	done := make(chan struct{})
	runtime := &Runtime{
		command:     &exec.Cmd{Process: &os.Process{Pid: 12345}},
		done:        done,
		signalGroup: func(int, syscall.Signal) error { return nil },
		probeGroup:  func(int) error { return syscall.ESRCH },
	}
	owner := make(chan error, 1)
	go func() { owner <- runtime.Stop(context.Background()) }()
	for {
		runtime.mu.Lock()
		started := runtime.stopAttempt != nil
		runtime.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runtime.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("coalesced Stop=%v", err)
	}
	close(done)
	if err := <-owner; err != nil {
		t.Fatalf("owner Stop=%v", err)
	}
}

func TestAbsenceConfirmationHonorsContextAndCanRetry(t *testing.T) {
	done := make(chan struct{})
	close(done)
	runtime := &Runtime{
		command:      &exec.Cmd{Process: &os.Process{Pid: 12345}},
		done:         done,
		exitObserved: true,
		probeGroup:   func(int) error { return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := runtime.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop=%v", err)
	}
	runtime.probeGroup = func(int) error { return syscall.ESRCH }
	if err := runtime.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop=%v", err)
	}
}

func TestChildEnvironmentRemovesOnlyCredentialAndPreservesPathNeighbors(t *testing.T) {
	executable := helper(t, "env")
	t.Setenv("ARIES_RUNTIME_KEY", "super-secret")
	t.Setenv("ARIES_RUNTIME_KEY_SUFFIX", "survives")
	t.Setenv("PATH", "/usr/bin:/bin")
	output := t.TempDir()
	runtime, err := New(Options{Executable: executable, ConfigPath: "config", OutputDir: output, BaseURL: "http://127.0.0.1:30000/v1", CredentialEnv: "ARIES_RUNTIME_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-runtime.Done()
	raw, err := os.ReadFile(filepath.Join(output, "sglang", "stdout.log"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "ARIES_RUNTIME_KEY=super-secret") || !strings.Contains(text, "ARIES_RUNTIME_KEY_SUFFIX=survives") {
		t.Fatalf("environment log=%q", text)
	}
	wantPath := "PATH=" + filepath.Dir(executable) + string(os.PathListSeparator) + "/usr/bin:/bin"
	if !strings.Contains(text, wantPath+"\n") {
		t.Fatalf("environment PATH missing %q in %q", wantPath, text)
	}
}

func TestCredentialNamedPathGetsExecutableOnlyPath(t *testing.T) {
	got := childEnvironment([]string{"PATH=/secret/bin", "PATH_SUFFIX=survives"}, "PATH", "/runtime/bin")
	if strings.Join(got, "\n") != "PATH_SUFFIX=survives\nPATH=/runtime/bin" {
		t.Fatalf("environment=%q", got)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("file %q was not created", path)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSignalOwnedRefusesTermAndKillAfterExitObserved(t *testing.T) {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(signal.String(), func(t *testing.T) {
			calls := 0
			runtime := &Runtime{
				exitObserved: true,
				done:         make(chan struct{}),
				signalGroup: func(int, syscall.Signal) error {
					calls++
					return nil
				},
			}
			signaled, err := runtime.signalOwned(12345, signal)
			if err != nil || signaled || calls != 0 {
				t.Fatalf("signaled=%t calls=%d err=%v", signaled, calls, err)
			}
		})
	}
}

func TestDeriveHealthURLRejectsAmbiguousInputs(t *testing.T) {
	if got, err := deriveHealthURL("https://host:30000/v1"); err != nil || got != "https://host:30000/health" {
		t.Fatalf("valid URL=%q err=%v", got, err)
	}
	for _, input := range []string{
		"http://host/v1#",
		"http://host/v1#fragment",
		"http://host/v1?query",
		"http://host/v%31",
		"http://user@host/v1",
		"http://host/v1/",
		"http://host/v1/models",
		"http://host/health",
	} {
		if _, err := deriveHealthURL(input); err == nil {
			t.Fatalf("accepted ambiguous URL %q", input)
		}
	}
}

func TestSGLangHealthRouteBoundsAndClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		retry  bool
	}{
		{"ready", 200, false},
		{"timeout", 408, true},
		{"too_early", 425, true},
		{"rate", 429, true},
		{"server", 500, true},
		{"unavailable", 503, true},
		{"terminal", 404, false},
		{"redirect", 302, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seenPath := ""
			body := &controlledBody{reader: strings.NewReader("")}
			runtime := &Runtime{options: Options{BaseURL: "http://example.invalid/health"}, client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				seenPath = r.URL.Path
				if r.Header.Get("Authorization") != "" {
					t.Error("authorization sent")
				}
				return &http.Response{StatusCode: tc.status, Body: body, Header: make(http.Header)}, nil
			})}}
			err := runtime.Health(context.Background())
			if tc.status == 200 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var health *healthError
				if !errors.As(err, &health) || health.Retryable() != tc.retry {
					t.Fatalf("err=%v", err)
				}
			}
			if seenPath != "/health" {
				t.Fatalf("path=%q", seenPath)
			}
			if !body.closed {
				t.Fatal("response body was not closed")
			}
		})
	}
}

func TestHealthRejectsTransportReadCloseAndOversizeFailuresWithoutCanaries(t *testing.T) {
	canary := "health-body-canary"
	cases := []struct {
		name      string
		transport http.RoundTripper
		body      *controlledBody
	}{
		{name: "transport", transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New(canary) })},
		{name: "nil_response", transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, nil })},
		{name: "read", body: &controlledBody{readErr: errors.New(canary)}},
		{name: "close", body: &controlledBody{reader: strings.NewReader("ok"), closeErr: errors.New(canary)}},
		{name: "oversize", body: &controlledBody{reader: strings.NewReader(strings.Repeat("x", healthBodyMax+1))}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := tc.transport
			if tc.body != nil {
				transport = responseTransport(http.StatusOK, tc.body)
			}
			runtime := &Runtime{options: Options{BaseURL: "http://example.invalid/health"}, client: &http.Client{Transport: transport}}
			err := runtime.Health(context.Background())
			if err == nil || strings.Contains(err.Error(), canary) {
				t.Fatalf("err=%v", err)
			}
			var health *healthError
			if !errors.As(err, &health) {
				t.Fatalf("type=%T", err)
			}
			if tc.body != nil && !tc.body.closed {
				t.Fatal("response body was not closed")
			}
			wantRetry := tc.name == "transport" || tc.name == "nil_response"
			if health.Retryable() != wantRetry {
				t.Fatalf("retryable=%t want=%t", health.Retryable(), wantRetry)
			}
		})
	}
}

func TestHealthAcceptsHTTPClientNormalizedNilBody(t *testing.T) {
	runtime := &Runtime{options: Options{BaseURL: "http://example.invalid/health"}, client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, nil
	})}}
	if err := runtime.Health(context.Background()); err != nil {
		t.Fatalf("normalized nil body=%v", err)
	}
}

func TestHealthDistinguishesLocalAttemptTimeoutFromParentCancellation(t *testing.T) {
	blockingTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})

	t.Run("local attempt timeout is retryable", func(t *testing.T) {
		runtime := &Runtime{
			options:       Options{BaseURL: "http://example.invalid/health"},
			client:        &http.Client{Transport: blockingTransport},
			healthTimeout: 10 * time.Millisecond,
		}
		assertHealthRetryable(t, runtime.Health(context.Background()), true)
	})

	t.Run("parent cancellation is terminal", func(t *testing.T) {
		started := make(chan struct{})
		runtime := &Runtime{
			options: Options{BaseURL: "http://example.invalid/health"},
			client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				close(started)
				<-request.Context().Done()
				return nil, request.Context().Err()
			})},
			healthTimeout: time.Second,
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { result <- runtime.Health(ctx) }()
		<-started
		cancel()
		assertHealthRetryable(t, <-result, false)
	})

	t.Run("parent cancellation overrides a retryable response", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		runtime := &Runtime{
			options: Options{BaseURL: "http://example.invalid/health"},
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				cancel()
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			})},
			healthTimeout: time.Second,
		}
		assertHealthRetryable(t, runtime.Health(ctx), false)
	})

	t.Run("parent deadline is terminal", func(t *testing.T) {
		runtime := &Runtime{
			options:       Options{BaseURL: "http://example.invalid/health"},
			client:        &http.Client{Transport: blockingTransport},
			healthTimeout: time.Second,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		assertHealthRetryable(t, runtime.Health(ctx), false)
	})

	t.Run("pre-canceled parent is terminal and makes no request", func(t *testing.T) {
		called := false
		runtime := &Runtime{
			options: Options{BaseURL: "http://example.invalid/health"},
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				called = true
				return nil, errors.New("unexpected request")
			})},
			healthTimeout: time.Second,
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		assertHealthRetryable(t, runtime.Health(ctx), false)
		if called {
			t.Fatal("pre-canceled health made a request")
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type controlledBody struct {
	reader   io.Reader
	readErr  error
	closeErr error
	closed   bool
}

func (body *controlledBody) Read(buffer []byte) (int, error) {
	if body.readErr != nil {
		return 0, body.readErr
	}
	if body.reader == nil {
		return 0, io.EOF
	}
	return body.reader.Read(buffer)
}

func (body *controlledBody) Close() error {
	body.closed = true
	return body.closeErr
}

func responseTransport(status int, body io.ReadCloser) http.RoundTripper {
	return roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
	})
}

func assertHealthRetryable(t *testing.T, err error, want bool) {
	t.Helper()
	var health *healthError
	if !errors.As(err, &health) || health.Retryable() != want {
		t.Fatalf("health=%v retryable=%t want=%t", err, health != nil && health.Retryable(), want)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%04o want=%04o", path, got, want)
	}
}

func countOpenFileDescriptors(path string) int {
	links, _ := filepath.Glob("/proc/self/fd/*")
	count := 0
	for _, link := range links {
		target, err := os.Readlink(link)
		if err == nil && target == path {
			count++
		}
	}
	return count
}
