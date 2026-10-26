package ssh

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

// TestToolCallRecordKeepsTheSSHKeyOrder pins the SSH record layout: the
// common fields come first in their old order, request_type and want_reply
// stay the last two keys, and no gRPC-only key appears.
func TestToolCallRecordKeepsTheSSHKeyOrder(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	session := &bridgeSession{dialect: testDialect{},
		sandbox: &contractSandbox{acceptTools: true}, audit: bridgekit.NewAuditWriter(structured, nil),
		replyRequest: func(*ssh.Request, bool) error { return nil },
	}
	encoded := encodeCanonicalTokens([]string{remoteShell, "-c", "true"})
	requests := make(chan *ssh.Request, 1)
	requests <- &ssh.Request{Type: "exec", WantReply: true, Payload: ssh.Marshal(struct{ Command string }{encoded})}
	close(requests)
	session.handleSession(context.Background(), &stubSSHChannel{}, requests)
	if err := session.closeAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(structuredBytes.Bytes()))
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"sandbox_id", "sequence", "timestamp", "container_id", "container_name", "operation_class",
		"path", "workdir", "command_hash", "command", "argv", "stdin", "stdin_encoding", "stdin_bytes",
		"stdout_bytes", "stderr_bytes", "exit_code", "duration_ms", "status", "run_id", "task_id", "request_type", "want_reply"}
	if !slices.Equal(keys, want) {
		t.Fatalf("SSH record keys = %v, want %v", keys, want)
	}
}

func TestRawAuditUsesDeterministicLosslessHumanReadableGrammar(t *testing.T) {
	payload := append(ssh.Marshal(struct{ Command string }{"printf 'é && <tag>'"}), 0, 0xff, '\n', '\t', '\\')
	stdin := []byte("readable é\n--- ARIES SSH CALL END ---\x00\xff")
	record := rawRecord(requestAudit{requestType: "exec", wantReply: true, payload: payload, remoteCommand: "printf 'é && <tag>'"}, int64(len(stdin)), stdin, "completed")
	record.Sequence = 7
	record.Timestamp = "2026-07-24T12:00:00.000000123Z"
	record.RunID = "run"
	record.TaskID = "task"
	record.ContainerID = "container"
	content, err := renderRawSSHRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	parsed := decodeRawAuditRecords(t, content)
	if len(parsed) != 1 || parsed[0]["wire_command"] != "printf 'é && <tag>'" {
		t.Fatalf("raw records = %#v", parsed)
	}
	if !bytes.Equal(unescapeRawValue(t, parsed[0]["payload"]), payload) || !bytes.Equal(unescapeRawValue(t, parsed[0]["stdin"]), stdin) {
		t.Fatalf("raw round trip failed: %s", content)
	}
	if parsed[0]["payload_bytes"] != strconv.Itoa(len(payload)) || parsed[0]["stdin_bytes"] != strconv.Itoa(len(stdin)) || !bytes.Contains(content, []byte(`\xFF`)) {
		t.Fatalf("raw byte counts or uppercase escapes are wrong: %s", content)
	}
	if bytes.Contains(content, []byte("base64")) || bytes.Contains(content, []byte{'\x00'}) || bytes.Count(content, []byte("--- ARIES SSH CALL END ---\n")) != 1 {
		t.Fatalf("raw grammar is ambiguous or contains control bytes: %q", content)
	}
}

func TestManagerStopPropagatesAuditFailure(t *testing.T) {
	bad := &bridgekit.AuditFile{Write: func([]byte) (int, error) { return 0, errors.New("audit write") }, Sync: func() error { return nil }, Close: func() error { return nil }}
	good, _ := memoryAuditFile()
	session := &bridgeSession{dialect: testDialect{}, audit: bridgekit.NewAuditWriter(bad, good), connections: make(map[net.Conn]struct{})}
	session.audit.Enqueue(&bridgekit.ToolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
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
	session := &bridgeSession{dialect: testDialect{}, sandbox: sandbox, audit: bridgekit.NewAuditWriter(structured, raw)}
	channel := &stubSSHChannel{Buffer: *bytes.NewBuffer(bytes.Repeat([]byte{'x'}, bridgekit.MaxRecordedInputBytes+1))}
	encoded := encodeCanonicalTokens([]string{remoteShell, "-c", "cat"})
	prepared, _ := (testDialect{}).Prepare(encoded, sandbox.Workdir())
	if exit := session.execute(context.Background(), channel, prepared, requestAudit{requestType: "exec", wantReply: true, payload: ssh.Marshal(struct{ Command string }{encoded})}); exit != 255 {
		t.Fatalf("exit = %d", exit)
	}
	session.connections = make(map[net.Conn]struct{})
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
	session := &bridgeSession{dialect: testDialect{},
		sandbox: sandbox, audit: bridgekit.NewAuditWriter(structured, raw),
		replyRequest: func(*ssh.Request, bool) error { return errors.New("reply failed") },
	}
	encoded := encodeCanonicalTokens([]string{remoteShell, "-c", "true"})
	payload := ssh.Marshal(struct{ Command string }{encoded})
	requests := make(chan *ssh.Request, 1)
	requests <- &ssh.Request{Type: "exec", WantReply: true, Payload: payload}
	close(requests)
	session.handleSession(context.Background(), &stubSSHChannel{}, requests)
	if err := session.closeAudit(context.Background()); err != nil {
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
	session := &bridgeSession{dialect: testDialect{},
		sandbox: sandbox, audit: bridgekit.NewAuditWriter(structured, raw),
		replyRequest: func(*ssh.Request, bool) error { return nil },
	}
	payload := []byte{0, 0, 0, 7, 'b', 'a', 'd', 0xff}
	requests := make(chan *ssh.Request, 1)
	requests <- &ssh.Request{Type: "exec", WantReply: true, Payload: payload}
	close(requests)
	session.handleSession(context.Background(), &stubSSHChannel{}, requests)
	if err := session.closeAudit(context.Background()); err != nil {
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

func memoryAuditFile() (*bridgekit.AuditFile, *bytes.Buffer) {
	var mu sync.Mutex
	var buffer bytes.Buffer
	return &bridgekit.AuditFile{
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
	fields := []string{"sequence", "sandbox_id", "timestamp", "request_type", "want_reply", "status", "run_id", "task_id", "container_id", "wire_command", "payload_bytes", "payload", "stdin_bytes", "stdin"}
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
	endpoint, err := manager.StartTarget(context.Background(), sandbox)
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

	broken := newContractManager(t, t.TempDir())
	broken.openAudit = func(string) (*bridgekit.AuditFile, error) {
		return nil, errors.New("injected audit allocation failure")
	}
	failedSandbox := &contractSandbox{}
	if _, err := broken.StartTarget(context.Background(), failedSandbox); err == nil {
		t.Fatal("partial Start unexpectedly succeeded")
	}
	failedSandbox.mu.Lock()
	failedPreparations := append([]core.Command(nil), failedSandbox.preparations...)
	failedSandbox.mu.Unlock()
	if len(failedPreparations) != 0 {
		t.Fatalf("partial start executed workspace cleanup commands: %#v", failedPreparations)
	}
}

func TestLatePartialStartCleansListenerAndAudit(t *testing.T) {
	outputDir := t.TempDir()
	manager := newContractManager(t, outputDir)
	sandbox := &contractSandbox{}
	want := errors.New("injected late start failure")
	var address, artifactDir string
	var retainedPaths []string
	manager.afterStart = func(session *bridgeSession) error {
		if session.listener == nil || session.audit == nil {
			t.Fatal("late start hook ran before listener and audit allocation")
		}
		address = session.listener.Addr().String()
		artifactDir = session.artifactDir
		retainedPaths = []string{
			session.toolLogPath,
			session.rawLogPath,
		}
		return want
	}
	if _, err := manager.StartTarget(context.Background(), sandbox); !errors.Is(err, want) {
		t.Fatalf("Start() error = %v, want %v", err, want)
	}
	if connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("late partial-start listener still accepts connections")
	}
	for _, path := range retainedPaths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("partial-start audit path remains %q: %v", path, err)
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
	endpoint, err := manager.StartTarget(context.Background(), sandbox)
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
	endpoint, err := manager.StartTarget(context.Background(), sandbox)
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
	endpoint, err := manager.StartTarget(context.Background(), sandbox)
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

type revokedExecSandbox struct {
	cancelingSandbox
	result core.CommandResult
	err    error
}

func (sandbox *revokedExecSandbox) ExecStream(ctx context.Context, _ core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
	sandbox.once.Do(func() { close(sandbox.started) })
	<-ctx.Done()
	close(sandbox.canceled)
	return sandbox.result, sandbox.err
}

func TestStopPreservesToolResultAfterCancellation(t *testing.T) {
	transportErr := errors.New("exec transport closed")
	for _, test := range []struct {
		name     string
		err      error
		status   string
		exitCode int
	}{
		{name: "canceled", err: context.Canceled, status: "canceled", exitCode: 255},
		{name: "canceled with transport error", err: errors.Join(context.Canceled, transportErr), status: "canceled", exitCode: 255},
		{name: "transport error", err: transportErr, status: "failed", exitCode: 255},
		{name: "completed", status: "completed", exitCode: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			outputDir := t.TempDir()
			manager := newContractManager(t, outputDir)
			sandbox := &revokedExecSandbox{
				cancelingSandbox: cancelingSandbox{started: make(chan struct{}), canceled: make(chan struct{})},
				result:           core.CommandResult{ExitCode: 7},
				err:              test.err,
			}
			endpoint, err := manager.StartTarget(context.Background(), sandbox)
			if err != nil {
				t.Fatal(err)
			}
			client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
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
			if err := manager.Stop(stopCtx); err != nil {
				t.Fatalf("Stop() failed after joining the canceled execution: %v", err)
			}
			connection, err := net.DialTimeout("tcp", endpoint.Address, time.Second)
			if err == nil {
				_ = connection.Close()
				t.Fatal("revoked bridge still accepts connections")
			}
			records, content := readToolCallRecords(t, outputDir)
			if len(records) != 1 {
				t.Fatalf("tool log records = %d, want one: %s", len(records), content)
			}
			assertLogString(t, records[0], "status", test.status)
			if got := records[0]["exit_code"]; got != float64(test.exitCode) {
				t.Fatalf("recorded exit code = %v, want %d", got, test.exitCode)
			}
			for _, secret := range []string{stdinSecret, envSecret} {
				if bytes.Contains(content, []byte(secret)) {
					t.Fatalf("tool log contains secret %q: %s", secret, content)
				}
			}
		})
	}
}

func TestByteCounterTracksConcurrentPipeTraffic(t *testing.T) {
	const chunks = 128
	payload := bytes.Repeat([]byte("late-stream-content"), 32)
	want := int64(chunks * len(payload))

	readPipe, writePipe := io.Pipe()
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

func TestRecordedInputKeepsRawAndUsesSafeStructuredEncoding(t *testing.T) {
	for _, test := range []struct {
		name, want, encoding string
		content              []byte
	}{
		{name: "utf8", content: []byte("actual stdin\n"), want: "actual stdin\n", encoding: "utf-8"},
		{name: "binary", content: []byte{0, 0xff}, want: "[binary input omitted; 2 bytes retained in ssh_raw.log]", encoding: "binary-omitted"},
		{name: "utf8 control", content: []byte("prefix\x00suffix"), want: "[binary input omitted; 13 bytes retained in ssh_raw.log]", encoding: "binary-omitted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := &recordedInput{reader: bytes.NewReader(test.content)}
			if _, err := io.Copy(io.Discard, input); err != nil {
				t.Fatal(err)
			}
			count, content, encoding, raw, overflow := input.record(true)
			if count != int64(len(test.content)) || content != test.want || encoding != test.encoding || !bytes.Equal(raw, test.content) || overflow {
				t.Fatalf("record = %d %q %q", count, content, encoding)
			}
		})
	}
	t.Run("bounded", func(t *testing.T) {
		input := &recordedInput{reader: io.LimitReader(zeroReader{}, bridgekit.MaxRecordedInputBytes+1)}
		if _, err := io.Copy(io.Discard, input); err == nil || !strings.Contains(err.Error(), "stdin exceeds") {
			t.Fatalf("oversized stdin error = %v", err)
		}
		count, content, encoding, raw, overflow := input.record(true)
		if count <= bridgekit.MaxRecordedInputBytes || content != "" || encoding != "utf-8" || len(raw) != 0 || !overflow {
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
			count, content, encoding, raw, overflow := input.record(true)
			if encoding != "utf-8" || count != int64(len(content)) || !bytes.Equal(raw, []byte(content)) || overflow {
				t.Fatalf("final snapshot = count %d content %d encoding %q", count, len(content), encoding)
			}
			return
		default:
			count, content, encoding, raw, overflow := input.record(true)
			if encoding != "utf-8" || count != int64(len(content)) || !bytes.Equal(raw, []byte(content)) || overflow {
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

func TestManagerCreatesOnlyEvidenceAndCannotReuseRevokedOccurrence(t *testing.T) {
	manager := newContractManager(t, t.TempDir())
	endpoint, err := manager.StartTarget(context.Background(), &contractSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.ClientSourceFile != "" {
		t.Fatalf("server staged client files: %#v", endpoint)
	}
	entries, err := os.ReadDir(manager.active.artifactDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "tool-calls.jsonl" && entry.Name() != "ssh_raw.log" {
			t.Fatalf("unexpected server artifact %s", entry.Name())
		}
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.hostSigner != nil {
		t.Fatal("revoked manager retained host signer")
	}
	if _, err := manager.StartTarget(context.Background(), &contractSandbox{}); err == nil {
		t.Fatal("revoked occurrence reused")
	}
}

func TestManagerStopTimeoutRetainsSessionUntilJoined(t *testing.T) {
	session := &bridgeSession{connections: make(map[net.Conn]struct{})}
	session.wait.Add(1)
	manager := &Manager{active: session, used: true}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := manager.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v", err)
	}
	if manager.active != session {
		t.Fatal("timed out Stop abandoned session")
	}
	session.wait.Done()
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if manager.active != nil {
		t.Fatal("joined session remains active")
	}
}

func TestStopBeforeStartPermanentlyRevokesOccurrence(t *testing.T) {
	for _, failedStart := range []bool{false, true} {
		t.Run(strconv.FormatBool(failedStart), func(t *testing.T) {
			manager := newContractManager(t, t.TempDir())
			if failedStart {
				manager.resolveListen = func(context.Context) (core.BridgeListen, error) {
					return core.BridgeListen{}, errors.New("listener resolution failed")
				}
				if _, err := manager.StartTarget(context.Background(), &contractSandbox{}); err == nil {
					t.Fatal("failed resolution accepted")
				}
				manager.resolveListen = loopbackListen
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if manager.hostSigner != nil {
				t.Fatal("Stop retained host signer reference")
			}
			if _, err := manager.StartTarget(context.Background(), &contractSandbox{}); err == nil {
				t.Fatal("revoked occurrence started")
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type invalidActionDialect struct{ testDialect }

func (invalidActionDialect) Prepare(string, string) (Prepared, *Refusal) {
	return Prepared{Action: Action(255)}, nil
}
func TestInvalidPreparedActionCannotExecute(t *testing.T) {
	structured, content := memoryAuditFile()
	sandbox := &contractSandbox{acceptTools: true}
	accepted := false
	session := &bridgeSession{dialect: invalidActionDialect{}, sandbox: sandbox, audit: bridgekit.NewAuditWriter(structured, nil), replyRequest: func(_ *ssh.Request, ok bool) error { accepted = ok; return nil }}
	requests := make(chan *ssh.Request, 1)
	requests <- &ssh.Request{Type: "exec", WantReply: true, Payload: ssh.Marshal(struct{ Command string }{"command"})}
	close(requests)
	session.handleSession(context.Background(), &stubSSHChannel{}, requests)
	if err := session.closeAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if accepted || len(sandbox.snapshot()) != 0 {
		t.Fatal("invalid action admitted execution")
	}
	records := decodeAuditLines(t, content.Bytes())
	if len(records) != 1 || records[0]["status"] != "rejected" {
		t.Fatalf("refusal evidence = %#v", records)
	}
}
