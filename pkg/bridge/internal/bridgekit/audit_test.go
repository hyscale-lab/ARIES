package bridgekit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const testLimit = 256 << 20

// testRecord stands in for a bridge's own record type.
type testRecord struct {
	Stamp
	Command     string   `json:"command,omitempty"`
	Argv        []string `json:"argv,omitempty"`
	Status      string   `json:"status"`
	RequestType string   `json:"request_type,omitempty"`
	WantReply   bool     `json:"want_reply,omitempty"`
}

// testSession is the smallest bridge session: no transport to revoke.
type testSession struct {
	Session
	closes atomic.Int32
}

func (session *testSession) Close(ctx context.Context) error {
	session.closes.Add(1)
	return session.Finish(ctx)
}

func TestAuditWriterPersistsConcurrentGapFreeCorrelatedRecords(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := newWriter(structured, raw, testLimit)
	const records = 50
	var wait sync.WaitGroup
	for i := range records {
		wait.Add(1)
		go func() {
			defer wait.Done()
			payload := []byte{0, byte(i), 0xff}
			writer.Enqueue(&testRecord{Status: "completed", RequestType: "exec", WantReply: true}, &RawRecord{
				RequestType: "exec", WantReply: true, WireCommand: "command", Payload: payload, PayloadBytes: int64(len(payload)),
				Stdin: payload, StdinBytes: int64(len(payload)), Status: "completed",
			}, 0)
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
	writer := newWriter(structured, raw, testLimit)
	want := "&& <tag> > é 漢字 \"quote\" \\slash\nline\t\x00"
	writer.Enqueue(&testRecord{Command: want, Status: "completed"}, &RawRecord{}, 0)
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
	payload := append([]byte("\x00\x00\x00\x13printf 'é && <tag>'"), 0, 0xff, '\n', '\t', '\\')
	stdin := []byte("readable é\n--- ARIES SSH CALL END ---\x00\xff")
	content := renderRawRecord(RawRecord{
		Sequence: 7, Timestamp: "2026-07-24T12:00:00.000000123Z", RequestType: "exec", WantReply: true,
		Status: "completed", RunID: "run", TaskID: "task", ContainerID: "container",
		WireCommand: "printf 'é && <tag>'", Payload: payload, PayloadBytes: int64(len(payload)),
		Stdin: stdin, StdinBytes: int64(len(stdin)),
	})
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
			writer := newWriter(structured, raw, testLimit)
			writer.Enqueue(&testRecord{Status: "completed"}, &RawRecord{Status: "completed"}, 0)
			<-blocked
			done := make(chan struct{})
			go func() {
				for range 100 {
					writer.Enqueue(&testRecord{Status: "completed"}, &RawRecord{Status: "completed"}, 0)
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
			writer := newWriter(structured, raw, testLimit)
			writer.Enqueue(&testRecord{Status: "completed"}, &RawRecord{Status: "completed"}, 0)
			if err := writer.sealAndWait(context.Background()); err == nil {
				t.Fatal("expected persistence error")
			}
		})
	}
}

func TestAuditWriterRejectsRecordsAfterAdmissionFailure(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := newWriter(structured, raw, testLimit)
	marshalCalls := 0
	writer.marshal = func(value any) ([]byte, error) {
		marshalCalls++
		if marshalCalls == 1 {
			return nil, errors.New("marshal")
		}
		return json.Marshal(value)
	}

	writer.Enqueue(&testRecord{Status: "failed"}, &RawRecord{Status: "failed"}, 0)
	writer.Enqueue(&testRecord{Status: "completed"}, &RawRecord{Status: "completed"}, 0)
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
	latched := make(chan struct{})
	var structuredWrites, rawWrites atomic.Int32
	structured := &auditFile{
		write: func([]byte) (int, error) {
			structuredWrites.Add(1)
			return 0, errors.New("persist failed")
		},
		sync:  func() error { return nil },
		close: func() error { return nil },
	}
	raw := &auditFile{
		write: func(content []byte) (int, error) {
			rawWrites.Add(1)
			close(latched)
			return len(content), nil
		},
		sync:  func() error { return nil },
		close: func() error { return nil },
	}
	writer := newWriter(structured, raw, testLimit)
	writer.Enqueue(&testRecord{Status: "failed"}, &RawRecord{Status: "failed"}, 0)
	select {
	case <-latched:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for persistence failure to be latched")
	}

	writer.mu.Lock()
	latchedErr := writer.err
	writer.mu.Unlock()
	if latchedErr == nil || !strings.Contains(latchedErr.Error(), "persist failed") {
		t.Fatalf("latched error = %v, want persistence failure", latchedErr)
	}
	writer.Enqueue(&testRecord{Status: "completed"}, &RawRecord{Status: "completed"}, 0)

	slot := &Slot{active: &testSession{Session: Session{Audit: writer}}}
	for attempt := 0; attempt < 2; attempt++ {
		if err := slot.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "persist failed") {
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
	structuredRecord := testRecord{Status: "completed", Argv: argv}
	rawRecord := RawRecord{Status: "completed", Payload: []byte{0, 1}, PayloadBytes: 2, Stdin: []byte{2, 3}, StdinBytes: 2}
	structuredCandidate := structuredRecord
	rawCandidate := rawRecord
	structuredCandidate.Sequence, rawCandidate.Sequence = 1, 1
	structuredCandidate.Timestamp, rawCandidate.Timestamp = fixed.Format(time.RFC3339Nano), fixed.Format(time.RFC3339Nano)
	structuredLine, _ := marshalJSONLine(structuredCandidate)
	rawLine := renderRawRecord(rawCandidate)
	charge := int64(len(structuredLine) + len(rawLine))

	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := newWriter(structured, raw, testLimit)
	writer.now = func() time.Time { return fixed }
	writer.bytes = testLimit - charge
	writer.Enqueue(&structuredRecord, &rawRecord, 0)
	argv[1] = "mutated-after-enqueue"
	if err := writer.sealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.bytes != testLimit || writer.sequence != 1 {
		t.Fatalf("admission bytes/sequence = %d/%d", writer.bytes, writer.sequence)
	}
	if bytes.Contains(structuredBytes.Bytes(), []byte("mutated-after-enqueue")) || !bytes.Contains(structuredBytes.Bytes(), []byte("original")) || rawBytes.Len() == 0 {
		t.Fatalf("enqueue did not retain immutable pair: %s / %s", structuredBytes.Bytes(), rawBytes.Bytes())
	}

	structured, structuredBytes = memoryAuditFile()
	raw, rawBytes = memoryAuditFile()
	overflow := newWriter(structured, raw, testLimit)
	overflow.now = func() time.Time { return fixed }
	overflow.bytes = testLimit - charge + 1
	overflow.Enqueue(&structuredRecord, &rawRecord, 0)
	if err := overflow.sealAndWait(context.Background()); err == nil || overflow.sequence != 0 || structuredBytes.Len() != 0 || rawBytes.Len() != 0 {
		t.Fatalf("overflow = %v, sequence=%d, bytes=%d/%d", err, overflow.sequence, structuredBytes.Len(), rawBytes.Len())
	}
}

func TestAuditWriterLatchesMarshalAndEnqueueAfterSeal(t *testing.T) {
	structured, _ := memoryAuditFile()
	raw, _ := memoryAuditFile()
	marshalFailure := newWriter(structured, raw, testLimit)
	marshalFailure.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	marshalFailure.Enqueue(&testRecord{Status: "completed"}, &RawRecord{Status: "completed"}, 0)
	if err := marshalFailure.sealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("marshal error = %v", err)
	}

	structured, _ = memoryAuditFile()
	raw, _ = memoryAuditFile()
	afterSeal := newWriter(structured, raw, testLimit)
	if err := afterSeal.sealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterSeal.Enqueue(&testRecord{Status: "completed"}, &RawRecord{Status: "completed"}, 0)
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
	writer := newWriter(structured, raw, testLimit)
	writer.Enqueue(&testRecord{Status: "completed"}, &RawRecord{Status: "completed"}, 0)
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
