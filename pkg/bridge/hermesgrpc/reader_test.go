package hermesgrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"
	"testing/fstest"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
)

// grpcReaderRecord is the tool-calls.jsonl layout a reader written against this
// bridge today relies on. It is frozen on purpose and must never be edited to
// follow the production record: the bridge may add keys or reorder them, but
// every key below must stay present with the same JSON type.
type grpcReaderRecord struct {
	Sequence       uint64   `json:"sequence"`
	Timestamp      string   `json:"timestamp"`
	ContainerID    string   `json:"container_id"`
	ContainerName  string   `json:"container_name"`
	OperationClass string   `json:"operation_class"`
	Path           string   `json:"path"`
	Workdir        string   `json:"workdir"`
	CommandHash    string   `json:"command_hash"`
	Command        string   `json:"command"`
	Argv           []string `json:"argv"`
	Stdin          string   `json:"stdin"`
	StdinEncoding  string   `json:"stdin_encoding"`
	StdinRaw       string   `json:"stdin_raw"`
	ContentRaw     string   `json:"content_raw"`
	SHA256         string   `json:"sha256"`
	StdinBytes     int64    `json:"stdin_bytes"`
	StdoutBytes    int64    `json:"stdout_bytes"`
	Truncated      bool     `json:"truncated"`
	StderrBytes    int64    `json:"stderr_bytes"`
	ExitCode       int      `json:"exit_code"`
	DurationMS     int64    `json:"duration_ms"`
	Status         string   `json:"status"`
	Error          string   `json:"error"`
	RunID          string   `json:"run_id"`
	TaskID         string   `json:"task_id"`
}

// grpcReaderRequiredKeys are the keys present in every record today, so a
// reader may index them directly.
var grpcReaderRequiredKeys = []string{
	"sequence", "timestamp", "container_id", "container_name", "operation_class",
	"command_hash", "stdin", "stdin_encoding", "stdin_bytes", "stdout_bytes",
	"stderr_bytes", "exit_code", "duration_ms", "status",
}

func TestToolCallsStayReadableByTodaysReader(t *testing.T) {
	sandbox := newMemorySandbox(fstest.MapFS{})
	manager, client, endpoint := startFileBridge(t, sandbox, Options{RetainContent: true})
	ctx := context.Background()
	_, _ = client.Exec(ctx, &sandboxv1.ExecRequest{Script: agentPayload})
	_, _ = client.Exec(ctx, &sandboxv1.ExecRequest{Script: catPayload, Stdin: []byte{0, 1, 2}})
	_, _ = client.Exec(ctx, &sandboxv1.ExecRequest{Script: syncPayload})
	_, _ = client.Exec(ctx, &sandboxv1.ExecRequest{Script: garbagePayload})
	if _, err := writeFile(client, "/app/f", 3, []byte("a\nb")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/f"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/f", FirstLine: 1, MaxLines: 1, MaxLineBytes: 100}); err != nil {
		t.Fatal(err)
	}
	session := manager.active
	session.revoke()
	_ = session.authorize(agentPayload)
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	records := readGRPCReaderRecords(t, endpoint.LogPaths[0])
	want := []struct{ class, status string }{
		{"agent", "completed"},
		{"agent", "completed"},
		{"sync", "denied"},
		{"unknown", "rejected"},
		{"file_write", "completed"},
		{"file_read", "completed"},
		{"file_lines", "completed"},
		{"unknown", "rejected"},
	}
	if len(records) != len(want) {
		t.Fatalf("records = %#v", records)
	}
	for index, expected := range want {
		if records[index].OperationClass != expected.class || records[index].Status != expected.status {
			t.Fatalf("record %d = %#v, want %+v", index, records[index], expected)
		}
	}
	if records[0].Command != agentPayload || records[1].StdinRaw == "" || records[2].Command != syncPayload ||
		records[2].ExitCode != -1 || records[4].ContentRaw == "" || records[4].SHA256 == "" || records[7].Command != agentPayload {
		t.Fatalf("records = %#v", records)
	}
}

// readGRPCReaderRecords reads the log the way today's reader does: every
// required key present, every value decodable into the frozen types.
func readGRPCReaderRecords(t *testing.T, path string) []grpcReaderRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []grpcReaderRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &keys); err != nil {
			t.Fatal(err)
		}
		for _, key := range grpcReaderRequiredKeys {
			if _, ok := keys[key]; !ok {
				t.Fatalf("record %d lacks %q: %s", len(records), key, scanner.Text())
			}
		}
		var record grpcReaderRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("record %d does not decode into today's layout: %v", len(records), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}
