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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

func TestAuditWriterPersistsConcurrentGapFreeCorrelatedRecords(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := newAuditWriter(structured, raw)
	const records = 50
	var wait sync.WaitGroup
	for i := range records {
		wait.Add(1)
		go func() {
			defer wait.Done()
			payload := []byte{0, byte(i), 0xff}
			writer.enqueue(toolCallRecord{Status: "completed", RequestType: "exec", WantReply: true}, rawRecord(requestAudit{
				requestType: "exec", wantReply: true, payload: payload, remoteCommand: "command",
			}, int64(len(payload)), payload, "completed"))
		}()
	}
	wait.Wait()
	if err := writer.sealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	structuredRecords := decodeAuditLines(t, structuredBytes.Bytes())
	rawRecords := decodeRawAuditRecords(t, rawBytes.Bytes())
	if len(structuredRecords) != records || len(rawRecords) != records {
		t.Fatalf("record counts = %d, %d", len(structuredRecords), len(rawRecords))
	}
	for index := range records {
		want := index + 1
		if structuredRecords[index]["sequence"] != float64(want) || rawRecords[index]["sequence"] != strconv.Itoa(want) {
			t.Fatalf("sequence %d = %#v / %#v", index, structuredRecords[index], rawRecords[index])
		}
		payload := unescapeRawValue(t, rawRecords[index]["payload"])
		stdin := unescapeRawValue(t, rawRecords[index]["stdin"])
		if !bytes.Equal(stdin, payload) || len(payload) != 3 {
			t.Fatalf("raw exact bytes %d = payload %x stdin %x", index, payload, stdin)
		}
	}
}

func TestToolCallJSONLDisablesHTMLEscapingButKeepsRequiredEscapes(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, _ := memoryAuditFile()
	writer := newAuditWriter(structured, raw)
	want := "&& <tag> > é 漢字 \"quote\" \\slash\nline\t\x00"
	writer.enqueue(toolCallRecord{Command: want, Status: "completed"}, rawSSHRecord{})
	if err := writer.sealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	content := structuredBytes.Bytes()
	for _, literal := range [][]byte{[]byte("&&"), []byte("<tag>"), []byte("é"), []byte("漢字")} {
		if !bytes.Contains(content, literal) {
			t.Fatalf("structured JSON lacks literal %q: %s", literal, content)
		}
	}
	for _, forbidden := range [][]byte{[]byte(`\u0026`), []byte(`\u003c`), []byte(`\u003e`)} {
		if bytes.Contains(bytes.ToLower(content), forbidden) {
			t.Fatalf("structured JSON retained HTML escape %q: %s", forbidden, content)
		}
	}
	records := decodeAuditLines(t, content)
	if len(records) != 1 || records[0]["command"] != want {
		t.Fatalf("structured JSON round trip = %#v", records)
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

func TestAuditWriterDoesNotBlockEnqueueOnSlowStorage(t *testing.T) {
	for _, slowFile := range []string{"structured", "raw"} {
		t.Run(slowFile, func(t *testing.T) {
			blocked := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			slow := &auditFile{
				write: func(content []byte) (int, error) {
					if calls.Add(1) == 1 {
						close(blocked)
						<-release
					}
					return len(content), nil
				}, sync: func() error { return nil }, close: func() error { return nil },
			}
			fast, _ := memoryAuditFile()
			structured, raw := slow, fast
			if slowFile == "raw" {
				structured, raw = fast, slow
			}
			writer := newAuditWriter(structured, raw)
			writer.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
			<-blocked
			done := make(chan struct{})
			go func() {
				for range 100 {
					writer.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("enqueue blocked on storage")
			}
			close(release)
			if err := writer.sealAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAuditWriterLatchesAdmissionAndFileFailures(t *testing.T) {
	tests := map[string]func() (*auditFile, *auditFile){
		"write": func() (*auditFile, *auditFile) {
			bad := &auditFile{write: func([]byte) (int, error) { return 0, errors.New("write") }, sync: func() error { return nil }, close: func() error { return nil }}
			good, _ := memoryAuditFile()
			return bad, good
		},
		"short write": func() (*auditFile, *auditFile) {
			bad := &auditFile{write: func([]byte) (int, error) { return 0, nil }, sync: func() error { return nil }, close: func() error { return nil }}
			good, _ := memoryAuditFile()
			return bad, good
		},
		"sync": func() (*auditFile, *auditFile) {
			bad := &auditFile{write: func(content []byte) (int, error) { return len(content), nil }, sync: func() error { return errors.New("sync") }, close: func() error { return nil }}
			good, _ := memoryAuditFile()
			return bad, good
		},
		"close": func() (*auditFile, *auditFile) {
			bad := &auditFile{write: func(content []byte) (int, error) { return len(content), nil }, sync: func() error { return nil }, close: func() error { return errors.New("close") }}
			good, _ := memoryAuditFile()
			return bad, good
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			structured, raw := files()
			writer := newAuditWriter(structured, raw)
			writer.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
			if err := writer.sealAndWait(context.Background()); err == nil {
				t.Fatal("expected persistence error")
			}
		})
	}
}

func TestAuditWriterRejectsRecordsAfterAdmissionFailure(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := newAuditWriter(structured, raw)
	marshalCalls := 0
	writer.marshal = func(value any) ([]byte, error) {
		marshalCalls++
		if marshalCalls == 1 {
			return nil, errors.New("marshal")
		}
		return json.Marshal(value)
	}

	writer.enqueue(toolCallRecord{Status: "failed"}, rawSSHRecord{Status: "failed"})
	writer.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
	if err := writer.sealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("sealAndWait() error = %v, want retained admission failure", err)
	}
	if marshalCalls != 1 {
		t.Fatalf("marshal calls = %d, want no admission after failure", marshalCalls)
	}
	if structuredBytes.Len() != 0 || rawBytes.Len() != 0 {
		t.Fatalf("records persisted after failure: structured=%q raw=%q", structuredBytes.Bytes(), rawBytes.Bytes())
	}
}

func TestAuditWriterRejectsRecordsAfterPersistenceFailureAndStopRetainsError(t *testing.T) {
	failed := make(chan struct{})
	var structuredWrites, rawWrites atomic.Int32
	structured := &auditFile{
		write: func([]byte) (int, error) {
			structuredWrites.Add(1)
			close(failed)
			return 0, errors.New("persist failed")
		},
		sync:  func() error { return nil },
		close: func() error { return nil },
	}
	raw := &auditFile{
		write: func(content []byte) (int, error) {
			rawWrites.Add(1)
			return len(content), nil
		},
		sync:  func() error { return nil },
		close: func() error { return nil },
	}
	writer := newAuditWriter(structured, raw)
	writer.enqueue(toolCallRecord{Status: "failed"}, rawSSHRecord{Status: "failed"})
	<-failed

	writer.mu.Lock()
	latched := writer.err
	writer.mu.Unlock()
	if latched == nil || !strings.Contains(latched.Error(), "persist failed") {
		t.Fatalf("latched error = %v, want persistence failure", latched)
	}
	writer.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})

	session := &bridgeSession{audit: writer, connections: make(map[net.Conn]struct{})}
	manager := &Manager{active: session}
	for attempt := 0; attempt < 2; attempt++ {
		if err := manager.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "persist failed") {
			t.Fatalf("Stop() attempt %d error = %v, want retained persistence failure", attempt+1, err)
		}
	}
	if structuredWrites.Load() != 1 || rawWrites.Load() != 1 || writer.sequence != 1 {
		t.Fatalf("writes structured/raw=%d/%d sequence=%d, want only first pair admitted", structuredWrites.Load(), rawWrites.Load(), writer.sequence)
	}
}

func TestAuditWriterExactCombinedBudgetBoundaryAndImmutableEnqueue(t *testing.T) {
	fixed := time.Date(2026, 7, 24, 12, 0, 0, 123, time.UTC)
	argv := []string{"/bin/sh", "original"}
	structuredRecord := toolCallRecord{Status: "completed", Argv: argv}
	rawRecord := rawSSHRecord{Status: "completed", Payload: []byte{0, 1}, PayloadBytes: 2, Stdin: []byte{2, 3}, StdinBytes: 2}
	structuredCandidate := structuredRecord
	rawCandidate := rawRecord
	structuredCandidate.Sequence, rawCandidate.Sequence = 1, 1
	structuredCandidate.Timestamp, rawCandidate.Timestamp = fixed.Format(time.RFC3339Nano), fixed.Format(time.RFC3339Nano)
	structuredLine, _ := marshalJSONLine(structuredCandidate)
	rawLine, _ := renderRawSSHRecord(rawCandidate)
	charge := int64(len(structuredLine) + len(rawLine))

	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := newAuditWriter(structured, raw)
	writer.now = func() time.Time { return fixed }
	writer.bytes = maxToolLogBytes - charge
	writer.enqueue(structuredRecord, rawRecord)
	argv[1] = "mutated-after-enqueue"
	if err := writer.sealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.bytes != maxToolLogBytes || writer.sequence != 1 {
		t.Fatalf("admission bytes/sequence = %d/%d", writer.bytes, writer.sequence)
	}
	if bytes.Contains(structuredBytes.Bytes(), []byte("mutated-after-enqueue")) || !bytes.Contains(structuredBytes.Bytes(), []byte("original")) || rawBytes.Len() == 0 {
		t.Fatalf("enqueue did not retain immutable pair: %s / %s", structuredBytes.Bytes(), rawBytes.Bytes())
	}

	structured, structuredBytes = memoryAuditFile()
	raw, rawBytes = memoryAuditFile()
	overflow := newAuditWriter(structured, raw)
	overflow.now = func() time.Time { return fixed }
	overflow.bytes = maxToolLogBytes - charge + 1
	overflow.enqueue(structuredRecord, rawRecord)
	if err := overflow.sealAndWait(context.Background()); err == nil || overflow.sequence != 0 || structuredBytes.Len() != 0 || rawBytes.Len() != 0 {
		t.Fatalf("overflow = %v, sequence=%d, bytes=%d/%d", err, overflow.sequence, structuredBytes.Len(), rawBytes.Len())
	}
}

func TestAuditWriterLatchesMarshalAndEnqueueAfterSeal(t *testing.T) {
	structured, _ := memoryAuditFile()
	raw, _ := memoryAuditFile()
	marshalFailure := newAuditWriter(structured, raw)
	marshalFailure.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	marshalFailure.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
	if err := marshalFailure.sealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("marshal error = %v", err)
	}

	structured, _ = memoryAuditFile()
	raw, _ = memoryAuditFile()
	renderFailure := newAuditWriter(structured, raw)
	renderFailure.renderRaw = func(rawSSHRecord) ([]byte, error) { return nil, errors.New("render") }
	renderFailure.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
	if err := renderFailure.sealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "render") {
		t.Fatalf("render error = %v", err)
	}

	structured, _ = memoryAuditFile()
	raw, _ = memoryAuditFile()
	afterSeal := newAuditWriter(structured, raw)
	if err := afterSeal.sealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterSeal.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
	if err := afterSeal.sealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "after seal") {
		t.Fatalf("enqueue-after-seal error = %v", err)
	}
}

func TestAuditWriterDrainTimeoutIsRetryable(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	structured := &auditFile{
		write: func(content []byte) (int, error) { close(blocked); <-release; return len(content), nil },
		sync:  func() error { return nil }, close: func() error { return nil },
	}
	raw, _ := memoryAuditFile()
	writer := newAuditWriter(structured, raw)
	writer.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
	<-blocked
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := writer.sealAndWait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v", err)
	}
	close(release)
	if err := writer.sealAndWait(context.Background()); err != nil {
		t.Fatalf("retry drain = %v", err)
	}
}

func TestManagerStopPropagatesAuditFailure(t *testing.T) {
	bad := &auditFile{write: func([]byte) (int, error) { return 0, errors.New("audit write") }, sync: func() error { return nil }, close: func() error { return nil }}
	good, _ := memoryAuditFile()
	session := &bridgeSession{audit: newAuditWriter(bad, good), connections: make(map[net.Conn]struct{})}
	session.audit.enqueue(toolCallRecord{Status: "completed"}, rawSSHRecord{Status: "completed"})
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
	session := &bridgeSession{sandbox: sandbox, audit: newAuditWriter(structured, raw)}
	channel := &stubSSHChannel{Buffer: *bytes.NewBuffer(bytes.Repeat([]byte{'x'}, maxRecordedInputBytes+1))}
	encoded := encodeCanonicalTokens([]string{remoteShell, "-c", "cat"})
	remote, err := decodeRemoteCommand(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if exit := session.execute(context.Background(), channel, encoded, remote, requestAudit{requestType: "exec", wantReply: true, payload: ssh.Marshal(struct{ Command string }{encoded})}); exit != 255 {
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
	session := &bridgeSession{
		sandbox: sandbox, audit: newAuditWriter(structured, raw),
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
	session := &bridgeSession{
		sandbox: sandbox, audit: newAuditWriter(structured, raw),
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

func memoryAuditFile() (*auditFile, *bytes.Buffer) {
	var mu sync.Mutex
	var buffer bytes.Buffer
	return &auditFile{
		write: func(content []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return buffer.Write(content) },
		sync:  func() error { return nil }, close: func() error { return nil },
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

func TestPartialStartAuditDrainTimeoutRetainsArtifactsForStopRetry(t *testing.T) {
	outputDir := t.TempDir()
	manager := newContractManager(t, outputDir)
	manager.cleanupTimeout = 10 * time.Millisecond
	started := make(chan struct{})
	release := make(chan struct{})
	var opened int
	var once sync.Once
	manager.openAudit = func(path string) (*auditFile, error) {
		file, err := openAuditFile(path)
		if err != nil {
			return nil, err
		}
		if opened == 0 {
			originalSync := file.sync
			file.sync = func() error {
				once.Do(func() { close(started) })
				<-release
				return originalSync()
			}
		}
		opened++
		return file, nil
	}
	sandbox := &releaseFailSandbox{err: errors.New("release failed")}
	_, err := manager.Start(context.Background(), sandbox)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start() error = %v", err)
	}
	<-started
	artifactDir := filepath.Join(outputDir, sandbox.TaskID(), "bridge")
	if _, err := os.Stat(filepath.Join(artifactDir, "ssh_raw.log")); err != nil {
		t.Fatalf("raw audit removed after drain timeout: %v", err)
	}
	manager.mu.Lock()
	retained := manager.active != nil
	manager.mu.Unlock()
	if !retained {
		t.Fatal("partial session was not retained for Stop retry")
	}
	close(release)
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() retry = %v", err)
	}
	if _, err := os.Stat(artifactDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial artifact directory remains after retry: %v", err)
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
			count, content, encoding, raw, overflow := input.record()
			if count != int64(len(test.content)) || content != test.want || encoding != test.encoding || !bytes.Equal(raw, test.content) || overflow {
				t.Fatalf("record = %d %q %q", count, content, encoding)
			}
		})
	}
	t.Run("bounded", func(t *testing.T) {
		input := &recordedInput{reader: io.LimitReader(zeroReader{}, maxRecordedInputBytes+1)}
		if _, err := io.Copy(io.Discard, input); err == nil || !strings.Contains(err.Error(), "stdin exceeds") {
			t.Fatalf("oversized stdin error = %v", err)
		}
		count, content, encoding, raw, overflow := input.record()
		if count <= maxRecordedInputBytes || content != "" || encoding != "utf-8" || len(raw) != 0 || !overflow {
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
			count, content, encoding, raw, overflow := input.record()
			if encoding != "utf-8" || count != int64(len(content)) || !bytes.Equal(raw, []byte(content)) || overflow {
				t.Fatalf("final snapshot = count %d content %d encoding %q", count, len(content), encoding)
			}
			return
		default:
			count, content, encoding, raw, overflow := input.record()
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
