package codexssh

import (
	"bytes"
	"context"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/sshbridge"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRPCAuditOverlappingProcessesAndEarlyExit(t *testing.T) {
	var records []sshbridge.ToolCallRecord
	var failure error
	observer := newRPCAudit(func(r sshbridge.ToolCallRecord) { records = append(records, r) }, func(e error) { failure = e })
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	observer.now = func() time.Time { return now }
	input := `{"id":1,"method":"process/start","params":{"processId":"p","argv":["echo","a b"],"cwd":"file:///app","metadata":{"threadId":"parent","toolCallId":"tool"}}}` + "\n"
	for _, b := range []byte(input) {
		observer.feed(true, []byte{b})
	}
	observer.feed(true, []byte(`{"id":"1","method":"process/start","params":{"processId":"c","argv":["false"],"metadata":{"threadId":"child"}}}`+"\n"))
	now = now.Add(time.Second)
	observer.feed(false, []byte(`{"method":"process/output","params":{"processId":"p","stream":"stdout","chunk":"aGk="}}`+"\n"+`{"method":"process/exited","params":{"processId":"p","exitCode":0}}`+"\n"))
	observer.feed(false, []byte(`{"method":"process/closed","params":{"processId":"p"}}`+"\n"))
	observer.feed(false, []byte(`{"id":1,"result":{"processId":"p"}}`+"\n"+`{"id":"1","error":{"code":-1,"message":"private detail"}}`+"\n"))
	observer.finish(false)
	if failure != nil || len(records) != 2 {
		t.Fatalf("records=%+v failure=%v", records, failure)
	}
	if records[0].DurationMS != 1000 || records[0].StdoutBytes != 2 || records[0].Argv[1] != "a b" || records[0].ThreadID != "parent" || records[0].Status != "completed" {
		t.Fatalf("record=%+v", records[0])
	}
	if records[1].Status != "start_failed" || records[1].ExitCode != -1 || records[1].ThreadID != "child" {
		t.Fatalf("record=%+v", records[1])
	}
}

func TestRPCAuditIncompleteAndBounds(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		var record sshbridge.ToolCallRecord
		observer := newRPCAudit(func(r sshbridge.ToolCallRecord) { record = r }, func(e error) { t.Fatal(e) })
		observer.feed(true, []byte(`{"id":0,"method":"process/start","params":{"processId":"p","argv":["sleep","100"]}}`+"\n"))
		observer.finish(canceled)
		expected := "incomplete"
		if canceled {
			expected = "canceled"
		}
		if record.Status != expected || record.ExitCode != -1 || record.FinishedAt != "" {
			t.Fatalf("record=%+v", record)
		}
	}
	for _, input := range []string{"invalid\n", strings.Repeat("x", maxRPCFrameBytes+1), "{\"id\":"} {
		var failure error
		observer := newRPCAudit(func(sshbridge.ToolCallRecord) {}, func(e error) { failure = e })
		observer.feed(true, []byte(input))
		observer.finish(false)
		if failure == nil {
			t.Fatal("missing parse failure")
		}
	}
}

// Observation failures must neither change forwarding nor allow a successful
// audit drain. The original input remains independently replayable.
func TestRPCAuditPassthroughAndFailureLatch(t *testing.T) {
	var structured bytes.Buffer
	audit := sshbridge.NewAuditWriter("Codex", &sshbridge.AuditFile{Write: structured.Write, Sync: func() error { return nil }, Close: func() error { return nil }}, nil)
	observer := newRPCAudit(func(r sshbridge.ToolCallRecord) { audit.Enqueue(r, sshbridge.RawRecord{}) }, audit.Latch)
	original := []byte("malformed\x00protocol\n")
	recorded := sshbridge.NewRecordedInput("Codex", rpcAuditReader{reader: bytes.NewReader(original), audit: observer})
	got, err := io.ReadAll(recorded)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("input changed: %q %v", got, err)
	}
	var output bytes.Buffer
	_, err = (rpcAuditWriter{writer: &output, audit: observer}).Write(original)
	if err != nil || !bytes.Equal(output.Bytes(), original) {
		t.Fatalf("output changed: %q %v", output.Bytes(), err)
	}
	_, _, _, raw, _ := recorded.Record(true)
	if !bytes.Equal(raw, original) {
		t.Fatal("original replay bytes changed")
	}
	observer.finish(false)
	if err := audit.SealAndWait(context.Background()); err == nil {
		t.Fatal("observation failure not latched")
	}
	if err := audit.SealAndWait(context.Background()); err == nil {
		t.Fatal("second drain lost failure")
	}
}

func TestRPCAuditConcurrentStreams(t *testing.T) {
	observer := newRPCAudit(func(sshbridge.ToolCallRecord) {}, func(e error) { t.Error(e) })
	var group sync.WaitGroup
	for _, input := range []bool{true, false} {
		group.Add(1)
		go func(input bool) {
			defer group.Done()
			for i := 0; i < 100; i++ {
				observer.feed(input, []byte(`{"method":"environment/status","params":{}}`+"\n"))
			}
		}(input)
	}
	group.Wait()
	observer.finish(false)
}

func TestRPCAuditExitBeforeOutputDrain(t *testing.T) {
	for _, closed := range []bool{false, true} {
		var records []sshbridge.ToolCallRecord
		observer := newRPCAudit(func(r sshbridge.ToolCallRecord) { records = append(records, r) }, func(err error) { t.Fatal(err) })
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		observer.now = func() time.Time { return now }
		observer.feed(true, []byte(`{"id":1,"method":"process/start","params":{"processId":"p","argv":["echo","hello"]}}`+"\n"))
		now = now.Add(time.Second)
		observer.feed(false, []byte(`{"method":"process/exited","params":{"processId":"p","exitCode":0}}`+"\n"))
		now = now.Add(time.Second)
		observer.feed(false, []byte(`{"method":"process/output","params":{"processId":"p","stream":"pty","chunk":"aGk="}}`+"\n"))
		if len(records) != 0 {
			t.Fatal("emitted before output drained")
		}
		if closed {
			observer.feed(false, []byte(`{"method":"process/closed","params":{"processId":"p"}}`+"\n"))
		}
		observer.finish(true)
		if len(records) != 1 || records[0].DurationMS != 1000 || records[0].StdoutBytes != 2 || records[0].Status != "completed" || records[0].ExitCode != 0 {
			t.Fatalf("records=%+v", records)
		}
	}
}

func TestRPCAuditClosedBeforeExitNotification(t *testing.T) {
	var records []sshbridge.ToolCallRecord
	observer := newRPCAudit(func(r sshbridge.ToolCallRecord) { records = append(records, r) }, func(err error) { t.Fatal(err) })
	observer.feed(true, []byte(`{"id":1,"method":"process/start","params":{"processId":"p","argv":["true"]}}`+"\n"))
	observer.feed(false, []byte(`{"method":"process/closed","params":{"processId":"p"}}`+"\n"))
	if len(records) != 0 {
		t.Fatal("closed alone claimed exit")
	}
	observer.feed(false, []byte(`{"method":"process/exited","params":{"processId":"p","exitCode":0}}`+"\n"))
	observer.finish(false)
	if len(records) != 1 || records[0].Status != "completed" {
		t.Fatalf("records=%+v", records)
	}
}
