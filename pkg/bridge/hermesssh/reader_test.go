package hermesssh

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgetest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshReaderRecord is the tool-calls.jsonl layout a reader written against this
// bridge today relies on. It is frozen on purpose and must never be edited to
// follow the production record: the bridge may add keys or reorder them, but
// every key below must stay present with the same JSON type.
type sshReaderRecord struct {
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
	StdinBytes     int64    `json:"stdin_bytes"`
	StdoutBytes    int64    `json:"stdout_bytes"`
	StderrBytes    int64    `json:"stderr_bytes"`
	ExitCode       int      `json:"exit_code"`
	DurationMS     int64    `json:"duration_ms"`
	Status         string   `json:"status"`
	Error          string   `json:"error"`
	RunID          string   `json:"run_id"`
	TaskID         string   `json:"task_id"`
	RequestType    string   `json:"request_type"`
	WantReply      bool     `json:"want_reply"`
}

// sshReaderRequiredKeys are the keys present in every record today, so a
// reader may index them directly.
var sshReaderRequiredKeys = []string{
	"sequence", "timestamp", "container_id", "container_name", "operation_class",
	"command_hash", "stdin", "stdin_encoding", "stdin_bytes", "stdout_bytes",
	"stderr_bytes", "exit_code", "duration_ms", "status", "request_type", "want_reply",
}

func TestToolCallsStayReadableByTodaysReader(t *testing.T) {
	outputDir := t.TempDir()
	manager := newTestManager(t, outputDir)
	sandbox := &bridgetest.TestSandbox{}
	sandbox.Result.ExitCode = 3
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, clientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	agent := "bash -c 'echo ok'"
	_, _, _ = runExec(t, client, agent, "text-input")
	_, _, _ = runExec(t, client, "bash -c cat", "\x00\x01binary")
	_, _, _ = runExec(t, client, "mkdir -p /root/.hermes", "")
	_, _, _ = runExec(t, client, "not a hermes payload", "")
	client.Close()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	records := readReaderRecords(t, filepath.Join(outputDir, "test-task", "bridge", "tool-calls.jsonl"))
	want := []struct{ requestType, class, status, command string }{
		{"env", "unknown", "unsupported", ""},
		{"exec", "agent", "completed", agent},
		{"env", "unknown", "unsupported", ""},
		{"exec", "agent", "completed", "bash -c cat"},
		{"env", "unknown", "unsupported", ""},
		{"exec", "sync", "denied", ""},
		{"env", "unknown", "unsupported", ""},
		{"exec", "unknown", "rejected", ""},
	}
	if len(records) != len(want) {
		t.Fatalf("records = %#v", records)
	}
	for index, expected := range want {
		record := records[index]
		if record.RequestType != expected.requestType || record.OperationClass != expected.class ||
			record.Status != expected.status || record.Command != expected.command {
			t.Fatalf("record %d = %#v, want %+v", index, record, expected)
		}
	}
	if records[1].ExitCode != 3 || records[1].Stdin != "text-input" || records[3].StdinEncoding != "binary-omitted" || records[5].ExitCode != -1 {
		t.Fatalf("records = %#v", records)
	}
}

// readReaderRecords reads the log the way today's reader does: every required
// key present, every value decodable into the frozen types.
func readReaderRecords(t *testing.T, path string) []sshReaderRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []sshReaderRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &keys); err != nil {
			t.Fatal(err)
		}
		for _, key := range sshReaderRequiredKeys {
			if _, ok := keys[key]; !ok {
				t.Fatalf("record %d lacks %q: %s", len(records), key, scanner.Text())
			}
		}
		var record sshReaderRecord
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
