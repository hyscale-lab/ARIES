package openclawssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

func TestManagerPreparesPinnedWorkspaceAndRollsItBackAfterPartialStart(t *testing.T) {
	sandbox := &contractSandbox{}
	manager := newContractManager(t, t.TempDir())
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	sandbox.mu.Lock()
	preparations := append([]core.Command(nil), sandbox.preparations...)
	sandbox.mu.Unlock()
	if len(preparations) != 2 || preparations[0].Path != remoteShell || !containsArgument(preparations[0].Args, openClawWorkspace) || !containsArgument(preparations[0].Args, sandbox.Workdir()) {
		t.Fatalf("workspace preparation calls = %#v", preparations)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpoint.LogPaths[0]); err != nil {
		t.Fatalf("tool log was not retained: %v", err)
	}

	badClient := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(badClient, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := New(Options{OutputDir: t.TempDir(), ClientPath: badClient})
	if err != nil {
		t.Fatal(err)
	}
	failedSandbox := &contractSandbox{}
	if _, err := broken.Start(context.Background(), failedSandbox); err == nil {
		t.Fatal("partial Start unexpectedly succeeded")
	}
	failedSandbox.mu.Lock()
	failedPreparations := append([]core.Command(nil), failedSandbox.preparations...)
	failedSandbox.mu.Unlock()
	if len(failedPreparations) != 2 || !containsArgument(failedPreparations[1].Args, workspaceRollbackScript) {
		t.Fatalf("workspace rollback calls = %#v", failedPreparations)
	}
}

type releaseFailSandbox struct {
	contractSandbox
	err error
}

func (sandbox *releaseFailSandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	result, err := sandbox.contractSandbox.Exec(ctx, command)
	if containsArgument(command.Args, workspaceReleaseScript) {
		return core.CommandResult{ExitCode: -1}, sandbox.err
	}
	return result, err
}

func TestWorkspaceOwnershipReleaseFailureRollsBackStart(t *testing.T) {
	want := errors.New("release failed")
	sandbox := &releaseFailSandbox{err: want}
	manager := newContractManager(t, t.TempDir())
	if _, err := manager.Start(context.Background(), sandbox); !errors.Is(err, want) {
		t.Fatalf("Start() error = %v, want release failure", err)
	}
	sandbox.mu.Lock()
	preparations := append([]core.Command(nil), sandbox.preparations...)
	sandbox.mu.Unlock()
	if len(preparations) != 3 || !containsArgument(preparations[2].Args, workspaceRollbackScript) {
		t.Fatalf("release failure did not trigger workspace rollback: %#v", preparations)
	}
}

func TestManagerAnswersOnlyOpenSSHKeepalives(t *testing.T) {
	manager := newContractManager(t, t.TempDir())
	sandbox := &contractSandbox{}
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil || !ok {
		t.Fatalf("keepalive reply = ok %v err %v", ok, err)
	}
	if ok, _, err := client.SendRequest("unknown@aries", true, nil); err != nil || ok {
		t.Fatalf("unknown reply = ok %v err %v", ok, err)
	}
	_ = client.Close()
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type cancelingSandbox struct {
	contractSandbox
	started  chan struct{}
	canceled chan struct{}
	once     sync.Once
}

func (sandbox *cancelingSandbox) ExecStream(ctx context.Context, _ core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
	sandbox.once.Do(func() { close(sandbox.started) })
	<-ctx.Done()
	close(sandbox.canceled)
	return core.CommandResult{ExitCode: -1}, ctx.Err()
}

func TestClosingSSHConnectionCancelsItsDockerExec(t *testing.T) {
	manager := newContractManager(t, t.TempDir())
	sandbox := &cancelingSandbox{started: make(chan struct{}), canceled: make(chan struct{})}
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	sandbox.enableToolCalls()
	remote := encodeCanonicalTokens([]string{remoteShell, "-c", "sleep forever"})
	if err := session.Start(remote); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sandbox.started:
	case <-time.After(time.Second):
		t.Fatal("sandbox exec did not start")
	}
	_ = client.Close()
	select {
	case <-sandbox.canceled:
	case <-time.After(time.Second):
		t.Fatal("sandbox exec context survived SSH connection close")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Stop(stopCtx); err != nil {
		t.Fatalf("Stop() treated confirmed cancellation as failed revocation: %v", err)
	}
}

type failingToolSandbox struct {
	contractSandbox
	err error
}

func (sandbox *failingToolSandbox) ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error) {
	return core.CommandResult{ExitCode: -1}, sandbox.err
}

func TestStopIgnoresPriorOrdinaryToolExecutionFailure(t *testing.T) {
	outputDir := t.TempDir()
	manager := newContractManager(t, outputDir)
	sandbox := &failingToolSandbox{err: errors.New("ordinary Docker exec transport failure")}
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	sandbox.enableToolCalls()
	err = session.Run(encodeCanonicalTokens([]string{remoteShell, "-c", "true"}))
	_ = client.Close()
	var exitError *ssh.ExitError
	if !errors.As(err, &exitError) || exitError.ExitStatus() != 255 {
		t.Fatalf("SSH Run() error = %v, want exit 255", err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() was poisoned by an ordinary tool failure: %v", err)
	}
	records, _ := readToolCallRecords(t, outputDir)
	if len(records) != 1 {
		t.Fatalf("tool log records = %d, want one", len(records))
	}
	assertLogString(t, records[0], "status", "failed")
}

type terminationFailSandbox struct {
	cancelingSandbox
	terminationErr error
}

func (sandbox *terminationFailSandbox) ExecStream(ctx context.Context, _ core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
	sandbox.once.Do(func() { close(sandbox.started) })
	<-ctx.Done()
	close(sandbox.canceled)
	return core.CommandResult{ExitCode: -1}, errors.Join(ctx.Err(), sandbox.terminationErr)
}

func TestStopFailsWhenCancellationCannotConfirmTargetedTermination(t *testing.T) {
	outputDir := t.TempDir()
	manager := newContractManager(t, outputDir)
	terminationErr := errors.New("targeted termination was not confirmed")
	sandbox := &terminationFailSandbox{
		cancelingSandbox: cancelingSandbox{started: make(chan struct{}), canceled: make(chan struct{})},
		terminationErr:   terminationErr,
	}
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	const stdinSecret = "revocation-stdin-secret"
	const envSecret = "revocation-env-secret"
	session.Stdin = strings.NewReader(stdinSecret)
	sandbox.enableToolCalls()
	remote := encodeCanonicalTokens([]string{remoteEnv, "ARIES_SECRET=" + envSecret, remoteShell, "-c", "cat", "openclaw-sandbox-fs"})
	if err := session.Start(remote); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sandbox.started:
	case <-time.After(time.Second):
		t.Fatal("sandbox exec did not start")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stopErr := manager.Stop(stopCtx)
	_ = client.Close()
	if !errors.Is(stopErr, context.Canceled) || !errors.Is(stopErr, terminationErr) {
		t.Fatalf("Stop() error = %v, want cancellation joined with termination failure", stopErr)
	}
	records, content := readToolCallRecords(t, outputDir)
	if len(records) != 1 {
		t.Fatalf("tool log records = %d, want one: %s", len(records), content)
	}
	assertLogString(t, records[0], "status", "canceled")
	for _, secret := range []string{stdinSecret, envSecret} {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("tool log contains secret %q: %s", secret, content)
		}
	}
}

type cancellationBlindSandbox struct {
	cancelingSandbox
	err error
}

func (sandbox *cancellationBlindSandbox) ExecStream(ctx context.Context, _ core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
	sandbox.once.Do(func() { close(sandbox.started) })
	<-ctx.Done()
	close(sandbox.canceled)
	return core.CommandResult{ExitCode: -1}, sandbox.err
}

func TestStopFailsClosedWhenCanceledSandboxOmitsCancellationCause(t *testing.T) {
	manager := newContractManager(t, t.TempDir())
	transportErr := errors.New("attach ended without termination confirmation")
	sandbox := &cancellationBlindSandbox{
		cancelingSandbox: cancelingSandbox{started: make(chan struct{}), canceled: make(chan struct{})},
		err:              transportErr,
	}
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	sandbox.enableToolCalls()
	if err := session.Start(encodeCanonicalTokens([]string{remoteShell, "-c", "sleep forever"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sandbox.started:
	case <-time.After(time.Second):
		t.Fatal("sandbox exec did not start")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stopErr := manager.Stop(stopCtx)
	_ = client.Close()
	if !errors.Is(stopErr, context.Canceled) || !errors.Is(stopErr, transportErr) {
		t.Fatalf("Stop() error = %v, want cancellation joined with ambiguous sandbox error", stopErr)
	}
}

func TestByteCounterTracksConcurrentPipeTraffic(t *testing.T) {
	const chunks = 128
	payload := bytes.Repeat([]byte("late-stream-content"), 32)
	want := int64(chunks * len(payload))

	readPipe, writePipe := io.Pipe()
	readCounter := &byteCounter{reader: readPipe}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, readCounter)
		readDone <- err
	}()
	stopReadPolling := pollCounter(readCounter)
	for range chunks {
		if _, err := writePipe.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := writePipe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	stopReadPolling()
	if got := readCounter.count(); got != want {
		t.Fatalf("read count = %d, want %d", got, want)
	}

	readPipe, writePipe = io.Pipe()
	writeCounter := &byteCounter{writer: writePipe}
	drainDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, readPipe)
		drainDone <- err
	}()
	stopWritePolling := pollCounter(writeCounter)
	for range chunks {
		if _, err := writeCounter.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := writePipe.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-drainDone; err != nil {
		t.Fatal(err)
	}
	stopWritePolling()
	if got := writeCounter.count(); got != want {
		t.Fatalf("write count = %d, want %d", got, want)
	}
}

func TestRecordedInputUsesLosslessEncoding(t *testing.T) {
	for _, test := range []struct {
		name, want, encoding string
		content              []byte
	}{
		{name: "utf8", content: []byte("actual stdin\n"), want: "actual stdin\n", encoding: "utf-8"},
		{name: "binary", content: []byte{0, 0xff}, want: "AP8=", encoding: "base64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := &recordedInput{reader: bytes.NewReader(test.content)}
			if _, err := io.Copy(io.Discard, input); err != nil {
				t.Fatal(err)
			}
			count, content, encoding := input.record()
			if count != int64(len(test.content)) || content != test.want || encoding != test.encoding {
				t.Fatalf("record = %d %q %q", count, content, encoding)
			}
		})
	}
	t.Run("bounded", func(t *testing.T) {
		input := &recordedInput{reader: io.LimitReader(zeroReader{}, maxRecordedInputBytes+1)}
		if _, err := io.Copy(io.Discard, input); err == nil || !strings.Contains(err.Error(), "stdin exceeds") {
			t.Fatalf("oversized stdin error = %v", err)
		}
		count, content, encoding := input.record()
		if count != maxRecordedInputBytes || len(content) != maxRecordedInputBytes || encoding != "utf-8" {
			t.Fatalf("bounded record = count %d content %d encoding %q", count, len(content), encoding)
		}
	})
}

func TestRecordedInputSnapshotsCountAndContentTogether(t *testing.T) {
	input := &recordedInput{reader: &singleByteReader{remaining: 1 << 16}}
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, input)
		done <- err
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			count, content, encoding := input.record()
			if encoding != "utf-8" || count != int64(len(content)) {
				t.Fatalf("final snapshot = count %d content %d encoding %q", count, len(content), encoding)
			}
			return
		default:
			count, content, encoding := input.record()
			if encoding != "utf-8" || count != int64(len(content)) {
				t.Fatalf("inconsistent snapshot = count %d content %d encoding %q", count, len(content), encoding)
			}
		}
	}
}

type singleByteReader struct{ remaining int }

func (reader *singleByteReader) Read(content []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	content[0] = 'x'
	reader.remaining--
	return 1, nil
}

type zeroReader struct{}

func (zeroReader) Read(content []byte) (int, error) {
	clear(content)
	return len(content), nil
}

func pollCounter(counter *byteCounter) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = counter.count()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func TestOperationClassUsesOnlyKnownOpenClawLabels(t *testing.T) {
	for label, want := range map[string]string{
		"openclaw-sandbox-upload": "workspace_upload",
		"openclaw-sandbox-fs":     "exec",
		"untrusted-label":         "exec",
	} {
		command := core.Command{Path: remoteShell, Args: []string{"-c", "true", label}}
		if got := operationClass(command); got != want {
			t.Fatalf("operationClass(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestReplayDisplayCommandOmitsDuplicatedUploadScript(t *testing.T) {
	execCommand := core.Command{Path: remoteShell, Args: []string{"-c", "git status"}}
	if got := replayDisplayCommand(execCommand); got != "git status" {
		t.Fatalf("exec display command = %q", got)
	}
	uploadCommand := core.Command{Path: remoteShell, Args: []string{"-c", "large helper", "openclaw-sandbox-upload"}}
	if got := replayDisplayCommand(uploadCommand); got != "" {
		t.Fatalf("upload display command duplicated argv: %q", got)
	}
}

func containsArgument(arguments []string, value string) bool {
	for _, argument := range arguments {
		if argument == value || strings.Contains(argument, value) {
			return true
		}
	}
	return false
}
