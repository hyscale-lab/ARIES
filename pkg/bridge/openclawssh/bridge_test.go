package openclawssh

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/sshbridge"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

func TestWriteExclusiveExactModesUnderRestrictiveUmask(t *testing.T) {
	const childMarker = "ARIES_TEST_RESTRICTIVE_UMASK_CHILD"
	if os.Getenv(childMarker) == "1" {
		syscall.Umask(0o077)
		source := os.Getenv("ARIES_TEST_EXECUTABLE_SOURCE")
		executable := os.Getenv("ARIES_TEST_EXECUTABLE_DESTINATION")
		private := os.Getenv("ARIES_TEST_PRIVATE_DESTINATION")
		content := []byte("#!/bin/sh\nexit 0\n")
		if err := stageExecutable(source, executable); err != nil {
			t.Fatal(err)
		}
		if err := sshbridge.WriteExclusivePrivate(private, content); err != nil {
			t.Fatal(err)
		}
		for path, wantMode := range map[string]os.FileMode{executable: 0o555, private: 0o600} {
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("%s content = %q", filepath.Base(path), got)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != wantMode {
				t.Fatalf("%s mode = %04o, want %04o", filepath.Base(path), info.Mode().Perm(), wantMode)
			}
		}
		return
	}

	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	content := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(source, content, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestWriteExclusiveExactModesUnderRestrictiveUmask$")
	command.Env = append(os.Environ(),
		childMarker+"=1",
		"ARIES_TEST_EXECUTABLE_SOURCE="+source,
		"ARIES_TEST_EXECUTABLE_DESTINATION="+filepath.Join(directory, "staged"),
		"ARIES_TEST_PRIVATE_DESTINATION="+filepath.Join(directory, "private"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restrictive-umask subprocess failed: %v\n%s", err, output)
	}
}

func TestManagerStopPropagatesAuditFailure(t *testing.T) {
	bad := &sshbridge.AuditFile{Write: func([]byte) (int, error) { return 0, errors.New("audit write") }, Sync: func() error { return nil }, Close: func() error { return nil }}
	good, _ := memoryAuditFile()
	session := &bridgeSession{audit: sshbridge.NewAuditWriter("OpenClaw", bad, good)}
	session.audit.Enqueue(sshbridge.ToolCallRecord{Status: "completed"}, sshbridge.RawRecord{Status: "completed"})
	manager := &Manager{active: session}
	if err := manager.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "audit write") {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := manager.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "audit write") {
		t.Fatalf("idempotent Stop() error = %v", err)
	}
}

func TestOversizedStdinEmitsNoPartialPairAndFailsAudit(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	sandbox := &contractSandbox{acceptTools: true}
	session := &bridgeSession{sandbox: sandbox, audit: sshbridge.NewAuditWriter("OpenClaw", structured, raw)}
	channel := &stubSSHChannel{Buffer: *bytes.NewBuffer(bytes.Repeat([]byte{'x'}, sshbridge.MaxRecordedInputBytes+1))}
	encoded := encodeCanonicalTokens([]string{remoteShell, "-c", "cat"})
	remote, err := decodeRemoteCommand(encoded)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareRemoteCommand(remote, sandbox.Workdir())
	if err != nil {
		t.Fatal(err)
	}
	if exit := session.execute(context.Background(), channel, prepared, requestAudit{requestType: "exec", wantReply: true, payload: ssh.Marshal(struct{ Command string }{encoded})}); exit != 255 {
		t.Fatalf("exit = %d", exit)
	}
	manager := &Manager{active: session}
	if err := manager.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("Stop() audit error = %v", err)
	}
	if structuredBytes.Len() != 0 || rawBytes.Len() != 0 {
		t.Fatalf("oversized stdin emitted partial pair: %q / %q", structuredBytes.Bytes(), rawBytes.Bytes())
	}
}

func TestAcceptedReplyFailureRetainsExactRequestEvidence(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	sandbox := &contractSandbox{}
	session := &bridgeSession{
		sandbox: sandbox, audit: sshbridge.NewAuditWriter("OpenClaw", structured, raw),
		replyRequest: func(*ssh.Request, bool) error { return errors.New("reply failed") },
	}
	encoded := encodeCanonicalTokens([]string{remoteShell, "-c", "true"})
	payload := ssh.Marshal(struct{ Command string }{encoded})
	requests := make(chan *ssh.Request, 1)
	requests <- &ssh.Request{Type: "exec", WantReply: true, Payload: payload}
	close(requests)
	session.handleSession(context.Background(), &stubSSHChannel{}, requests)
	if err := session.audit.SealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	structuredRecords := decodeAuditLines(t, structuredBytes.Bytes())
	rawRecords := decodeRawAuditRecords(t, rawBytes.Bytes())
	if len(structuredRecords) != 1 || len(rawRecords) != 1 || structuredRecords[0]["status"] != "failed" || rawRecords[0]["status"] != "failed" {
		t.Fatalf("reply failure records = %#v / %#v", structuredRecords, rawRecords)
	}
	decoded := unescapeRawValue(t, rawRecords[0]["payload"])
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("reply failure payload = %x", decoded)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("reply failure executed sandbox command")
	}
}

func TestRawAuditRetainsMalformedRequestPayloadWithEmptyWireCommand(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	sandbox := &contractSandbox{}
	session := &bridgeSession{
		sandbox: sandbox, audit: sshbridge.NewAuditWriter("OpenClaw", structured, raw),
		replyRequest: func(*ssh.Request, bool) error { return nil },
	}
	payload := []byte{0, 0, 0, 7, 'b', 'a', 'd', 0xff}
	requests := make(chan *ssh.Request, 1)
	requests <- &ssh.Request{Type: "exec", WantReply: true, Payload: payload}
	close(requests)
	session.handleSession(context.Background(), &stubSSHChannel{}, requests)
	if err := session.audit.SealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	structuredRecords := decodeAuditLines(t, structuredBytes.Bytes())
	rawRecords := decodeRawAuditRecords(t, rawBytes.Bytes())
	if len(structuredRecords) != 1 || structuredRecords[0]["status"] != "rejected" || len(rawRecords) != 1 {
		t.Fatalf("malformed records = %#v / %#v", structuredRecords, rawRecords)
	}
	if rawRecords[0]["wire_command"] != "" || !bytes.Equal(unescapeRawValue(t, rawRecords[0]["payload"]), payload) {
		t.Fatalf("malformed raw evidence = %#v", rawRecords[0])
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("malformed SSH payload executed sandbox command")
	}
}

type stubSSHChannel struct {
	bytes.Buffer
	stderr bytes.Buffer
}

func (*stubSSHChannel) Close() error                                   { return nil }
func (*stubSSHChannel) CloseWrite() error                              { return nil }
func (*stubSSHChannel) SendRequest(string, bool, []byte) (bool, error) { return true, nil }
func (channel *stubSSHChannel) Stderr() io.ReadWriter                  { return &channel.stderr }

func memoryAuditFile() (*sshbridge.AuditFile, *bytes.Buffer) {
	var mu sync.Mutex
	var buffer bytes.Buffer
	return &sshbridge.AuditFile{
		Write: func(content []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buffer.Write(content) },
		Sync:  func() error { return nil }, Close: func() error { return nil },
	}, &buffer
}

func decodeAuditLines(t *testing.T, content []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func decodeRawAuditRecords(t *testing.T, content []byte) []map[string]string {
	t.Helper()
	const begin = "--- ARIES SSH CALL BEGIN ---\n"
	const end = "--- ARIES SSH CALL END ---\n"
	fields := []string{"sequence", "timestamp", "request_type", "want_reply", "status", "run_id", "task_id", "container_id", "wire_command", "payload_bytes", "payload", "stdin_bytes", "stdin"}
	var records []map[string]string
	for len(content) > 0 {
		if !bytes.HasPrefix(content, []byte(begin)) {
			t.Fatalf("raw audit missing begin delimiter: %q", content)
		}
		content = content[len(begin):]
		record := make(map[string]string, len(fields))
		for _, field := range fields {
			newline := bytes.IndexByte(content, '\n')
			if newline < 0 {
				t.Fatalf("raw audit missing %s line ending: %q", field, content)
			}
			line := string(content[:newline])
			prefix := field + "="
			if !strings.HasPrefix(line, prefix) {
				t.Fatalf("raw audit field order: got %q want prefix %q", line, prefix)
			}
			record[field] = strings.TrimPrefix(line, prefix)
			content = content[newline+1:]
		}
		if !bytes.HasPrefix(content, []byte(end)) {
			t.Fatalf("raw audit missing end delimiter: %q", content)
		}
		content = content[len(end):]
		records = append(records, record)
	}
	return records
}

func unescapeRawValue(t *testing.T, value string) []byte {
	t.Helper()
	var output []byte
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			_, size := utf8.DecodeRuneInString(value[index:])
			output = append(output, value[index:index+size]...)
			index += size
			continue
		}
		if index+1 >= len(value) {
			t.Fatalf("dangling raw escape in %q", value)
		}
		switch value[index+1] {
		case '\\':
			output = append(output, '\\')
			index += 2
		case 'n':
			output = append(output, '\n')
			index += 2
		case 'r':
			output = append(output, '\r')
			index += 2
		case 't':
			output = append(output, '\t')
			index += 2
		case 'x':
			if index+4 > len(value) {
				t.Fatalf("short raw hex escape in %q", value)
			}
			decoded, err := hex.DecodeString(value[index+2 : index+4])
			if err != nil {
				t.Fatalf("invalid raw hex escape in %q: %v", value, err)
			}
			output = append(output, decoded[0])
			index += 4
		default:
			t.Fatalf("unknown raw escape in %q", value)
		}
	}
	return output
}

func TestManagerStartNeverCreatesAWorkspaceAlias(t *testing.T) {
	sandbox := &contractSandbox{}
	manager := newContractManager(t, t.TempDir())
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	sandbox.mu.Lock()
	preparations := append([]core.Command(nil), sandbox.preparations...)
	sandbox.mu.Unlock()
	if len(preparations) != 0 {
		t.Fatalf("bridge start executed sandbox preparation commands: %#v", preparations)
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
	broken, err := New(Options{ResolveListen: loopbackListen, OutputDir: t.TempDir(), ClientPath: badClient})
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
	if len(failedPreparations) != 0 {
		t.Fatalf("partial start executed workspace cleanup commands: %#v", failedPreparations)
	}
}

func TestLatePartialStartCleansListenerCredentialsAndAuditWithoutAliasBranches(t *testing.T) {
	outputDir := t.TempDir()
	manager := newContractManager(t, outputDir)
	sandbox := &contractSandbox{}
	want := errors.New("injected late start failure")
	var address, artifactDir string
	var retainedPaths []string
	manager.afterStart = func(session *bridgeSession) error {
		if session.server == nil || session.audit == nil {
			t.Fatal("late start hook ran before listener and audit allocation")
		}
		address = session.server.Addr().String()
		artifactDir = session.artifactDir
		retainedPaths = []string{
			session.clientSource,
			session.identitySource,
			session.knownSource,
			session.toolLogPath,
			session.rawLogPath,
		}
		return want
	}
	if _, err := manager.Start(context.Background(), sandbox); !errors.Is(err, want) {
		t.Fatalf("Start() error = %v, want %v", err, want)
	}
	if connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("late partial-start listener still accepts connections")
	}
	for _, path := range retainedPaths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial-start credential/audit path remains %q: %v", path, err)
		}
	}
	if _, err := os.Lstat(artifactDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial-start artifact directory remains: %v", err)
	}
	manager.mu.Lock()
	active := manager.active
	manager.mu.Unlock()
	if active != nil {
		t.Fatal("successfully cleaned partial session was retained as active")
	}
	sandbox.mu.Lock()
	preparations := append([]core.Command(nil), sandbox.preparations...)
	sandbox.mu.Unlock()
	if len(preparations) != 0 {
		t.Fatalf("partial-start cleanup executed alias commands: %#v", preparations)
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

func TestOperationClassUsesOnlyKnownOpenClawLabels(t *testing.T) {
	for name, test := range map[string]struct {
		command core.Command
		want    string
	}{
		"exact upload": {
			command: core.Command{Path: remoteShell, Args: []string{"-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot}},
			want:    "workspace_upload",
		},
		"upload label on other script": {
			command: core.Command{Path: remoteShell, Args: []string{"-c", "true", directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot}},
			want:    "exec",
		},
		"upload near target": {
			command: core.Command{Path: remoteShell, Args: []string{"-c", directoryUploadScript, directoryUploadLabel, virtualWorkspace, virtualRuntimeRoot}},
			want:    "exec",
		},
	} {
		if got := operationClass(test.command); got != test.want {
			t.Fatalf("operationClass(%q) = %q, want %q", name, got, test.want)
		}
	}
}

func TestReplayDisplayCommandOmitsDuplicatedUploadScript(t *testing.T) {
	execCommand := core.Command{Path: remoteShell, Args: []string{"-c", "git status"}}
	if got := replayDisplayCommand(execCommand); got != "git status" {
		t.Fatalf("exec display command = %q", got)
	}
	uploadCommand := core.Command{Path: remoteShell, Args: []string{"-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot}}
	if got := replayDisplayCommand(uploadCommand); got != "" {
		t.Fatalf("upload display command duplicated argv: %q", got)
	}
}
