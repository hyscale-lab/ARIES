// Package bridgetest holds the test helpers the bridge packages share. Only
// tests import it; each helper keeps its original name, exported.
package bridgetest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/core"
)

// TestSandbox implements a bridge's sandbox capability with no transport
// dependency, including the Block channel an in-flight cancellation test needs.
type TestSandbox struct {
	Mu       sync.Mutex
	Commands []core.Command
	Stdins   [][]byte
	Result   core.CommandResult
	Block    chan struct{}
}

func (sandbox *TestSandbox) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	command.Args = append([]string(nil), command.Args...)
	command.Env = maps.Clone(command.Env)
	sandbox.Mu.Lock()
	defer sandbox.Mu.Unlock()
	sandbox.Commands = append(sandbox.Commands, command)
	return sandbox.Result, nil
}

func (sandbox *TestSandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	content, err := io.ReadAll(stdin)
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	if sandbox.Block != nil {
		select {
		case <-sandbox.Block:
		case <-ctx.Done():
			return core.CommandResult{ExitCode: -1}, ctx.Err()
		}
	}
	sandbox.Mu.Lock()
	sandbox.Stdins = append(sandbox.Stdins, content)
	sandbox.Mu.Unlock()
	result, err := sandbox.Exec(ctx, command)
	if err == nil {
		_, _ = io.WriteString(stdout, result.Stdout)
		_, _ = io.WriteString(stderr, result.Stderr)
	}
	return result, err
}

func (*TestSandbox) Upload(context.Context, string, string) error   { return nil }
func (*TestSandbox) Download(context.Context, string, string) error { return nil }
func (*TestSandbox) ContainerID() string                            { return "sandbox-container-id" }
func (*TestSandbox) ContainerName() string                          { return "sandbox-container-name" }
func (*TestSandbox) NetworkName() string                            { return "sandbox-network-name" }
func (*TestSandbox) NetworkGateway(context.Context) (string, error) { return "127.0.0.1", nil }
func (*TestSandbox) Workdir() string                                { return "/app" }
func (*TestSandbox) RunID() string                                  { return "test-run" }
func (*TestSandbox) TaskID() string                                 { return "test-task" }

func (sandbox *TestSandbox) Snapshot() []core.Command {
	sandbox.Mu.Lock()
	defer sandbox.Mu.Unlock()
	return append([]core.Command(nil), sandbox.Commands...)
}

func ReadToolCalls(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("tool-calls line is not JSON: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func RequireDocker(t *testing.T, images ...string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, image := range images {
		if output, err := exec.CommandContext(ctx, "docker", "image", "inspect", image).CombinedOutput(); err != nil {
			t.Skipf("pinned image is not present locally (%s): %s", image, output)
		}
	}
}

func DecodeAuditLines(t *testing.T, content []byte) []map[string]any {
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

func DecodeRawAuditRecords(t *testing.T, content []byte) []map[string]string {
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

func UnescapeRawValue(t *testing.T, value string) []byte {
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
