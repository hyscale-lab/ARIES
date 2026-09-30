package openclawssh

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// ocReaderRecord is the tool-calls.jsonl layout a reader written against this
// bridge today relies on. It is frozen on purpose and must never be edited to
// follow the production record: the bridge may add keys or reorder them, but
// every key below must stay present with the same JSON type.
type ocReaderRecord struct {
	Sequence       uint64   `json:"sequence"`
	Timestamp      string   `json:"timestamp"`
	ContainerID    string   `json:"container_id"`
	ContainerName  string   `json:"container_name"`
	OperationClass string   `json:"operation_class"`
	Path           string   `json:"path"`
	Workdir        string   `json:"workdir"`
	WorkspaceHome  string   `json:"workspace_home"`
	Environment    []string `json:"env_names"`
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

// ocReaderRequiredKeys are the keys present in every record today, so a
// reader may index them directly.
var ocReaderRequiredKeys = []string{
	"sequence", "timestamp", "container_id", "container_name", "operation_class",
	"command_hash", "stdin", "stdin_encoding", "stdin_bytes", "stdout_bytes",
	"stderr_bytes", "exit_code", "duration_ms", "status", "request_type", "want_reply",
}

func TestToolCallsStayReadableByTodaysReader(t *testing.T) {
	outputDir := t.TempDir()
	manager := newContractManager(t, outputDir)
	sandbox := &contractSandbox{}
	sandbox.result.ExitCode = 3
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	sandbox.enableToolCalls()
	client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	run := func(payload, stdin string) {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		session.Stdin = strings.NewReader(stdin)
		_ = session.Run(payload)
	}
	run(encodeCanonicalTokens([]string{remoteEnv, "LANG=C", remoteShell, "-c", "echo ok"}), "text-input")
	run(encodeCanonicalTokens([]string{remoteShell, "-c", "cat"}), "\x00\x01binary")
	run("not an openclaw payload", "")
	client.Close()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	records := readOCReaderRecords(t, filepath.Join(outputDir, "contract-task", "bridge", "tool-calls.jsonl"))
	if len(records) != 3 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Status != "completed" || records[0].ExitCode != 3 || records[0].Stdin != "text-input" ||
		len(records[0].Environment) != 1 || records[0].RequestType != "exec" || !records[0].WantReply {
		t.Fatalf("accepted record = %#v", records[0])
	}
	if records[1].Status != "completed" || records[1].StdinEncoding != "binary-omitted" {
		t.Fatalf("binary record = %#v", records[1])
	}
	if records[2].Status != "rejected" {
		t.Fatalf("rejected record = %#v", records[2])
	}
}

// readOCReaderRecords reads the log the way today's reader does: every
// required key present, every value decodable into the frozen types.
func readOCReaderRecords(t *testing.T, path string) []ocReaderRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []ocReaderRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &keys); err != nil {
			t.Fatal(err)
		}
		for _, key := range ocReaderRequiredKeys {
			if _, ok := keys[key]; !ok {
				t.Fatalf("record %d lacks %q: %s", len(records), key, scanner.Text())
			}
		}
		var record ocReaderRecord
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
