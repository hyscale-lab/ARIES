package bridgekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testRaw stands in for a transport's raw record. It renders its stamp and
// payload with the shared escaping.
type testRaw struct {
	payload []byte
	fail    bool
}

func (raw testRaw) RenderRaw(sequence uint64, timestamp string) ([]byte, error) {
	if raw.fail {
		return nil, errors.New("render")
	}
	var output bytes.Buffer
	WriteRawField(&output, "sequence", fmt.Sprint(sequence))
	WriteRawField(&output, "timestamp", timestamp)
	WriteRawBytesField(&output, "payload", raw.payload)
	return output.Bytes(), nil
}

func decodeTestRaw(t *testing.T, content []byte) []map[string]string {
	t.Helper()
	fields := []string{"sequence", "timestamp", "payload"}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines)%len(fields) != 0 {
		t.Fatalf("raw lines = %d", len(lines))
	}
	var records []map[string]string
	for len(lines) > 0 {
		record := map[string]string{}
		for index, field := range fields {
			value, ok := strings.CutPrefix(lines[index], field+"=")
			if !ok {
				t.Fatalf("raw field order: got %q want %s", lines[index], field)
			}
			record[field] = value
		}
		records = append(records, record)
		lines = lines[len(fields):]
	}
	return records
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

func TestAuditWriterPersistsConcurrentGapFreeCorrelatedRecords(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := NewAuditWriter(structured, raw)
	const records = 50
	var wait sync.WaitGroup
	for i := range records {
		wait.Add(1)
		go func() {
			defer wait.Done()
			payload := []byte{0, byte(i), 0xff}
			writer.Enqueue(&ToolCallRecord{Command: strconv.Itoa(i), Status: "completed"}, testRaw{payload: payload})
		}()
	}
	wait.Wait()
	if err := writer.SealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	structuredRecords := decodeAuditLines(t, structuredBytes.Bytes())
	rawRecords := decodeTestRaw(t, rawBytes.Bytes())
	if len(structuredRecords) != records || len(rawRecords) != records {
		t.Fatalf("record counts = %d, %d", len(structuredRecords), len(rawRecords))
	}
	for index := range records {
		want := index + 1
		if structuredRecords[index]["sequence"] != float64(want) || rawRecords[index]["sequence"] != strconv.Itoa(want) {
			t.Fatalf("sequence %d = %#v / %#v", index, structuredRecords[index], rawRecords[index])
		}
		if structuredRecords[index]["timestamp"] != rawRecords[index]["timestamp"] {
			t.Fatalf("timestamp %d = %#v / %#v", index, structuredRecords[index], rawRecords[index])
		}
		// The raw entry must belong to the structured record with the same sequence.
		item, _ := strconv.Atoi(structuredRecords[index]["command"].(string))
		var payload bytes.Buffer
		writeEscapedRaw(&payload, []byte{0, byte(item), 0xff})
		if rawRecords[index]["payload"] != payload.String() {
			t.Fatalf("raw payload %d = %q, want %q", index, rawRecords[index]["payload"], payload.String())
		}
	}
}

func TestToolCallJSONLDisablesHTMLEscapingButKeepsRequiredEscapes(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, _ := memoryAuditFile()
	writer := NewAuditWriter(structured, raw)
	want := "&& <tag> > é 漢字 \"quote\" \\slash\nline\t\x00"
	writer.Enqueue(&ToolCallRecord{Command: want, Status: "completed"}, testRaw{})
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
			writer := NewAuditWriter(structured, raw)
			writer.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{})
			<-blocked
			done := make(chan struct{})
			go func() {
				for range 100 {
					writer.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{})
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
			writer := NewAuditWriter(structured, raw)
			writer.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{})
			if err := writer.SealAndWait(context.Background()); err == nil {
				t.Fatal("expected persistence error")
			}
		})
	}
}

func TestAuditWriterRejectsRecordsAfterAdmissionFailure(t *testing.T) {
	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := NewAuditWriter(structured, raw)
	marshalCalls := 0
	writer.marshal = func(value any) ([]byte, error) {
		marshalCalls++
		if marshalCalls == 1 {
			return nil, errors.New("marshal")
		}
		return json.Marshal(value)
	}

	writer.Enqueue(&ToolCallRecord{Status: "failed"}, testRaw{})
	writer.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{})
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

func TestAuditWriterRejectsRecordsAfterPersistenceFailureAndStopRetainsError(t *testing.T) {
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
	writer := NewAuditWriter(structured, raw)
	writer.Enqueue(&ToolCallRecord{Status: "failed"}, testRaw{})
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
	writer.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{})

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
	rawRecord := testRaw{payload: []byte{0, 1}}
	structuredCandidate := structuredRecord
	structuredCandidate.Sequence = 1
	structuredCandidate.Timestamp = fixed.Format(time.RFC3339Nano)
	structuredLine, _ := marshalJSONLine(structuredCandidate)
	rawLine, _ := rawRecord.RenderRaw(1, fixed.Format(time.RFC3339Nano))
	charge := int64(len(structuredLine) + len(rawLine))

	structured, structuredBytes := memoryAuditFile()
	raw, rawBytes := memoryAuditFile()
	writer := NewAuditWriter(structured, raw)
	writer.now = func() time.Time { return fixed }
	writer.bytes = maxToolLogBytes - charge
	writer.Enqueue(&structuredRecord, rawRecord)
	argv[1] = "mutated-after-enqueue"
	if err := writer.SealAndWait(context.Background()); err != nil {
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
	overflow := NewAuditWriter(structured, raw)
	overflow.now = func() time.Time { return fixed }
	overflow.bytes = maxToolLogBytes - charge + 1
	overflow.Enqueue(&structuredRecord, rawRecord)
	if err := overflow.SealAndWait(context.Background()); err == nil || overflow.sequence != 0 || structuredBytes.Len() != 0 || rawBytes.Len() != 0 {
		t.Fatalf("overflow = %v, sequence=%d, bytes=%d/%d", err, overflow.sequence, structuredBytes.Len(), rawBytes.Len())
	}
}

func TestAuditWriterLatchesMarshalAndEnqueueAfterSeal(t *testing.T) {
	structured, _ := memoryAuditFile()
	raw, _ := memoryAuditFile()
	marshalFailure := NewAuditWriter(structured, raw)
	marshalFailure.marshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	marshalFailure.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{})
	if err := marshalFailure.SealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("marshal error = %v", err)
	}

	structured, _ = memoryAuditFile()
	raw, _ = memoryAuditFile()
	renderFailure := NewAuditWriter(structured, raw)
	renderFailure.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{fail: true})
	if err := renderFailure.SealAndWait(context.Background()); err == nil || !strings.Contains(err.Error(), "render") {
		t.Fatalf("render error = %v", err)
	}

	structured, _ = memoryAuditFile()
	raw, _ = memoryAuditFile()
	afterSeal := NewAuditWriter(structured, raw)
	if err := afterSeal.SealAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterSeal.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{})
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
	writer := NewAuditWriter(structured, raw)
	writer.Enqueue(&ToolCallRecord{Status: "completed"}, testRaw{})
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
