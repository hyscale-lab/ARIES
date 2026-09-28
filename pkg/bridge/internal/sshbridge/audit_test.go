package sshbridge

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

func TestAuditWriterPersistsConcurrentGapFreeCorrelatedRecords(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := NewAuditWriter("OpenClaw", structured, raw)
	const records = 50
	var wait sync.WaitGroup
	for i := range records {
		wait.Add(1)
		go func() {
			defer wait.Done()
			payload := []byte{0, byte(i), 0xff}
			writer.Enqueue(ToolCallRecord{Status: "completed", RequestType: "exec", WantReply: true}, RawRecord{RequestType: "exec", WantReply: true, Payload: payload, PayloadBytes: int64(len(payload)), WireCommand: "command", StdinBytes: int64(len(payload)), Stdin: payload, Status: "completed"})
		}()
	}
	wait.Wait()
	if err := writer.SealAndWait(context.Background()); err != nil {
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
	writer := NewAuditWriter("OpenClaw", structured, raw)
	want := "&& <tag> > é 漢字 \"quote\" \\slash\nline\t\x00"
	writer.Enqueue(ToolCallRecord{Command: want, Status: "completed"}, RawRecord{})
	if err := writer.SealAndWait(context.Background()); err != nil {
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
	record := RawRecord{RequestType: "exec", WantReply: true, Payload: payload, PayloadBytes: int64(len(payload)), WireCommand: "printf 'é && <tag>'", StdinBytes: int64(len(stdin)), Stdin: stdin, Status: "completed"}
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
			slow := &AuditFile{
				Write: func(content []byte) (int, error) {
					if calls.Add(1) == 1 {
						close(blocked)
						<-release
					}
					return len(content), nil
				}, Sync: func() error { return nil }, Close: func() error { return nil },
			}
			fast, _ := memoryAuditFile()
			structured, raw := slow, fast
			if slowFile == "raw" {
				structured, raw = fast, slow
			}
			writer := NewAuditWriter("OpenClaw", structured, raw)
			writer.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})
			<-blocked
			done := make(chan struct{})
			go func() {
				for range 100 {
					writer.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("enqueue blocked on storage")
			}
			close(release)
			if err := writer.SealAndWait(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAuditWriterLatchesAdmissionAndFileFailures(t *testing.T) {
	tests := map[string]func() (*AuditFile, *AuditFile){
		"write": func() (*AuditFile, *AuditFile) {
			bad := &AuditFile{Write: func([]byte) (int, error) { return 0, errors.New("write") }, Sync: func() error { return nil }, Close: func() error { return nil }}
			good, _ := memoryAuditFile()
			return bad, good
		},
		"short write": func() (*AuditFile, *AuditFile) {
			bad := &AuditFile{Write: func([]byte) (int, error) { return 0, nil }, Sync: func() error { return nil }, Close: func() error { return nil }}
			good, _ := memoryAuditFile()
			return bad, good
		},
		"sync": func() (*AuditFile, *AuditFile) {
			bad := &AuditFile{Write: func(content []byte) (int, error) { return len(content), nil }, Sync: func() error { return errors.New("sync") }, Close: func() error { return nil }}
			good, _ := memoryAuditFile()
			return bad, good
		},
		"close": func() (*AuditFile, *AuditFile) {
			bad := &AuditFile{Write: func(content []byte) (int, error) { return len(content), nil }, Sync: func() error { return nil }, Close: func() error { return errors.New("close") }}
			good, _ := memoryAuditFile()
			return bad, good
		},
	}
	for name, files := range tests {
		t.Run(name, func(t *testing.T) {
			structured, raw := files()
			writer := NewAuditWriter("OpenClaw", structured, raw)
			writer.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})
			if err := writer.SealAndWait(context.Background()); err == nil {
				t.Fatal("expected persistence error")
			}
		})
	}
}

func TestAuditWriterRejectsRecordsAfterAdmissionFailure(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := NewAuditWriter("OpenClaw", structured, raw)
	marshalCalls := 0
	writer.marshal = func(value any) ([]byte, error) {
		marshalCalls++
		if marshalCalls == 1 {
			return nil, errors.New("marshal")
		}
		return json.Marshal(value)
	}

	writer.Enqueue(ToolCallRecord{Status: "failed"}, RawRecord{Status: "failed"})
	writer.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})
	if err := writer.SealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("sealAndWait() error = %v, want retained admission failure", err)
	}
	if marshalCalls != 1 {
		t.Fatalf("marshal calls = %d, want no admission after failure", marshalCalls)
	}
	if structuredBytes.Len() != 0 || rawBytes.Len() != 0 {
		t.Fatalf("records persisted after failure: structured=%q raw=%q", structuredBytes.Bytes(), rawBytes.Bytes())
	}
}

func TestAuditWriterRejectsRecordsAfterPersistenceFailure(t *testing.T) {
	latched := make(chan struct{})
	var structuredWrites, rawWrites atomic.Int32
	structured := &AuditFile{
		Write: func([]byte) (int, error) {
			structuredWrites.Add(1)
			return 0, errors.New("persist failed")
		},
		Sync:  func() error { return nil },
		Close: func() error { return nil },
	}
	raw := &AuditFile{
		Write: func(content []byte) (int, error) {
			rawWrites.Add(1)
			close(latched)
			return len(content), nil
		},
		Sync:  func() error { return nil },
		Close: func() error { return nil },
	}
	writer := NewAuditWriter("OpenClaw", structured, raw)
	writer.Enqueue(ToolCallRecord{Status: "failed"}, RawRecord{Status: "failed"})
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
	writer.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})

	for attempt := 0; attempt < 2; attempt++ {
		if err := writer.SealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "persist failed") {
			t.Fatalf("SealAndWait() attempt %d error = %v, want retained persistence failure", attempt+1, err)
		}
	}
	if structuredWrites.Load() != 1 || rawWrites.Load() != 1 || writer.sequence != 1 {
		t.Fatalf("writes structured/raw=%d/%d sequence=%d, want only first pair admitted", structuredWrites.Load(), rawWrites.Load(), writer.sequence)
	}
}

func TestAuditWriterExactCombinedBudgetBoundaryAndImmutableEnqueue(t *testing.T) {
	fixed := time.Date(2026, 7, 24, 12, 0, 0, 123, time.UTC)
	argv := []string{"/bin/sh", "original"}
	structuredRecord := ToolCallRecord{Status: "completed", Argv: argv}
	rawRecord := RawRecord{Status: "completed", Payload: []byte{0, 1}, PayloadBytes: 2, Stdin: []byte{2, 3}, StdinBytes: 2}
	structuredCandidate := structuredRecord
	rawCandidate := rawRecord
	structuredCandidate.Sequence, rawCandidate.Sequence = 1, 1
	structuredCandidate.Timestamp, rawCandidate.Timestamp = fixed.Format(time.RFC3339Nano), fixed.Format(time.RFC3339Nano)
	structuredLine, _ := marshalJSONLine(structuredCandidate)
	rawLine, _ := renderRawSSHRecord(rawCandidate)
	charge := int64(len(structuredLine) + len(rawLine))

	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := NewAuditWriter("OpenClaw", structured, raw)
	writer.now = func() time.Time { return fixed }
	writer.bytes = MaxToolLogBytes - charge
	writer.Enqueue(structuredRecord, rawRecord)
	argv[1] = "mutated-after-enqueue"
	if err := writer.SealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if writer.bytes != MaxToolLogBytes || writer.sequence != 1 {
		t.Fatalf("admission bytes/sequence = %d/%d", writer.bytes, writer.sequence)
	}
	if bytes.Contains(structuredBytes.Bytes(), []byte("mutated-after-enqueue")) || !bytes.Contains(structuredBytes.Bytes(), []byte("original")) || rawBytes.Len() == 0 {
		t.Fatalf("enqueue did not retain immutable pair: %s / %s", structuredBytes.Bytes(), rawBytes.Bytes())
	}

	structured, structuredBytes = memoryAuditFile()
	raw, rawBytes = memoryAuditFile()
	overflow := NewAuditWriter("OpenClaw", structured, raw)
	overflow.now = func() time.Time { return fixed }
	overflow.bytes = MaxToolLogBytes - charge + 1
	overflow.Enqueue(structuredRecord, rawRecord)
	if err := overflow.SealAndWait(context.Background()); err == nil || overflow.sequence != 0 || structuredBytes.Len() != 0 || rawBytes.Len() != 0 {
		t.Fatalf("overflow = %v, sequence=%d, bytes=%d/%d", err, overflow.sequence, structuredBytes.Len(), rawBytes.Len())
	}
}

func TestAuditWriterLatchesMarshalAndEnqueueAfterSeal(t *testing.T) {
	structured, _ := memoryAuditFile()
	raw, _ := memoryAuditFile()
	marshalFailure := NewAuditWriter("OpenClaw", structured, raw)
	marshalFailure.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	marshalFailure.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})
	if err := marshalFailure.SealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("marshal error = %v", err)
	}

	structured, _ = memoryAuditFile()
	raw, _ = memoryAuditFile()
	renderFailure := NewAuditWriter("OpenClaw", structured, raw)
	renderFailure.renderRaw = func(RawRecord) ([]byte, error) { return nil, errors.New("render") }
	renderFailure.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})
	if err := renderFailure.SealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "render") {
		t.Fatalf("render error = %v", err)
	}

	structured, _ = memoryAuditFile()
	raw, _ = memoryAuditFile()
	afterSeal := NewAuditWriter("OpenClaw", structured, raw)
	if err := afterSeal.SealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterSeal.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})
	if err := afterSeal.SealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "after seal") {
		t.Fatalf("enqueue-after-seal error = %v", err)
	}
}

func TestAuditWriterDrainTimeoutIsRetryable(t *testing.T) {
	blocked := make(chan struct{})
	release := make(chan struct{})
	structured := &AuditFile{
		Write: func(content []byte) (int, error) { close(blocked); <-release; return len(content), nil },
		Sync:  func() error { return nil }, Close: func() error { return nil },
	}
	raw, _ := memoryAuditFile()
	writer := NewAuditWriter("OpenClaw", structured, raw)
	writer.Enqueue(ToolCallRecord{Status: "completed"}, RawRecord{Status: "completed"})
	<-blocked
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := writer.SealAndWait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v", err)
	}
	close(release)
	if err := writer.SealAndWait(context.Background()); err != nil {
		t.Fatalf("retry drain = %v", err)
	}
}

func memoryAuditFile() (*AuditFile, *bytes.Buffer) {
	var mu sync.Mutex
	var buffer bytes.Buffer
	return &AuditFile{
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

func TestByteCounterTracksConcurrentPipeTraffic(t *testing.T) {
	const chunks = 128
	payload := bytes.Repeat([]byte("late-stream-content"), 32)
	want := int64(chunks * len(payload))

	readPipe, writePipe := io.Pipe()
	readCounter := &ByteCounter{Reader: readPipe}
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
	if got := readCounter.Count(); got != want {
		t.Fatalf("read count = %d, want %d", got, want)
	}

	readPipe, writePipe = io.Pipe()
	writeCounter := &ByteCounter{Writer: writePipe}
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
	if got := writeCounter.Count(); got != want {
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
			input := NewRecordedInput("OpenClaw", bytes.NewReader(test.content))
			if _, err := io.Copy(io.Discard, input); err != nil {
				t.Fatal(err)
			}
			count, content, encoding, raw, overflow := input.Record(true)
			if count != int64(len(test.content)) || content != test.want || encoding != test.encoding || !bytes.Equal(raw, test.content) || overflow {
				t.Fatalf("record = %d %q %q", count, content, encoding)
			}
		})
	}
	t.Run("bounded", func(t *testing.T) {
		input := NewRecordedInput("OpenClaw", io.LimitReader(zeroReader{}, MaxRecordedInputBytes+1))
		if _, err := io.Copy(io.Discard, input); err == nil || !strings.Contains(err.Error(), "stdin exceeds") {
			t.Fatalf("oversized stdin error = %v", err)
		}
		count, content, encoding, raw, overflow := input.Record(true)
		if count <= MaxRecordedInputBytes || content != "" || encoding != "utf-8" || len(raw) != 0 || !overflow {
			t.Fatalf("bounded record = count %d content %d encoding %q", count, len(content), encoding)
		}
	})
}

func TestRecordedInputSnapshotsCountAndContentTogether(t *testing.T) {
	input := NewRecordedInput("OpenClaw", &singleByteReader{remaining: 1 << 16})
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
			count, content, encoding, raw, overflow := input.Record(true)
			if encoding != "utf-8" || count != int64(len(content)) || !bytes.Equal(raw, []byte(content)) || overflow {
				t.Fatalf("final snapshot = count %d content %d encoding %q", count, len(content), encoding)
			}
			return
		default:
			count, content, encoding, raw, overflow := input.Record(true)
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

func pollCounter(counter *ByteCounter) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				_ = counter.Count()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func TestBinaryStdinNoteMatchesRawRetention(t *testing.T) {
	for _, test := range []struct {
		name     string
		retained bool
		want     string
	}{
		{"retained", true, "retained in ssh_raw.log"},
		{"omitted", false, "not retained"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := NewRecordedInput("Hermes", bytes.NewReader([]byte{0, 1, 2}))
			if _, err := io.Copy(io.Discard, input); err != nil {
				t.Fatal(err)
			}
			_, note, encoding, _, _ := input.Record(test.retained)
			if encoding != "binary-omitted" || !strings.Contains(note, test.want) {
				t.Fatalf("binary note = %q (%s), want %q", note, encoding, test.want)
			}
		})
	}
}

func TestToolCallRecordKeepsExistingOptionalFields(t *testing.T) {
	content, err := marshalJSONLine(ToolCallRecord{})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"sequence":0,"timestamp":"","container_id":"","container_name":"","operation_class":"","command_hash":"","stdin":"","stdin_encoding":"","stdin_bytes":0,"stdout_bytes":0,"stderr_bytes":0,"exit_code":0,"duration_ms":0,"status":"","request_type":"","want_reply":false}` + "\n"
	if string(content) != want {
		t.Fatalf("minimal record changed: %s", content)
	}
	content, err = marshalJSONLine(ToolCallRecord{OperationClass: "exec", WorkspaceHome: "/app", Environment: []string{"PATH"}})
	if err != nil {
		t.Fatal(err)
	}
	records := decodeAuditLines(t, content)
	if records[0]["workspace_home"] != "/app" || records[0]["operation_class"] != "exec" {
		t.Fatalf("OpenClaw fields missing: %s", content)
	}
	names, ok := records[0]["env_names"].([]any)
	if !ok || len(names) != 1 || names[0] != "PATH" {
		t.Fatalf("environment names changed: %s", content)
	}
}

func TestOpenAuditFileIsPrivateAndExclusive(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "audit")
	file, err := OpenAuditFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("audit mode = %v, %v", info, err)
	}
	if _, err := OpenAuditFile(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing audit error = %v", err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAuditFile(link); !errors.Is(err, os.ErrExist) {
		t.Fatalf("symlink audit error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "first" {
		t.Fatalf("existing audit changed: %q, %v", got, err)
	}
}

func TestAuditWriterRetainsBridgeErrorPrefix(t *testing.T) {
	for _, label := range []string{"OpenClaw", "Hermes", "Codex"} {
		t.Run(label, func(t *testing.T) {
			structured, _ := memoryAuditFile()
			writer := NewAuditWriter(label, structured, nil)
			writer.bytes = MaxToolLogBytes
			writer.Enqueue(ToolCallRecord{}, RawRecord{})
			err := writer.SealAndWait(context.Background())
			if err == nil || !strings.Contains(err.Error(), label+" SSH combined audit exceeds") {
				t.Fatalf("bridge prefix = %v", err)
			}
		})
	}
}
