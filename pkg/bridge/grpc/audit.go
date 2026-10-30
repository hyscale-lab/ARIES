package grpc

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
)

// toolCallRecord is the gRPC line of tool-calls.jsonl.
type toolCallRecord struct {
	bridgekit.ToolCallRecord
	// SHA256 is the hex digest of the content a file call moved.
	SHA256 string `json:"sha256,omitempty"`
	// Truncated reports that the reply holds less than the sandbox produced.
	Truncated bool `json:"truncated,omitempty"`
	fileWindow
}

// fileWindow is the part of a file that a read asked for. A whole-file read,
// Stat and WriteFile have none of its fields.
type fileWindow struct {
	// MaxBytes is the ReadFile limit; zero reads the whole file.
	MaxBytes     int64 `json:"max_bytes,omitempty"`
	FirstLine    int64 `json:"first_line,omitempty"`
	MaxLines     int64 `json:"max_lines,omitempty"`
	MaxLineBytes int64 `json:"max_line_bytes,omitempty"`
	// TotalLines is set only when ReadLines produced a summary, so an empty
	// file records 0 and a failed call records nothing.
	TotalLines *int64 `json:"total_lines,omitempty"`
}

// rawGRPCRecord is one entry of grpc_raw.log. It keeps the Exec script and
// stdin and the written content; a read keeps only its path, size and digest.
type rawGRPCRecord struct {
	SandboxID      string
	OperationClass string
	Status         string
	RunID          string
	TaskID         string
	ContainerID    string
	Path           string
	Login          bool
	Script         string
	Stdin          []byte
	StdinBytes     int64
	ReadBytes      int64
	SHA256         string
}

func (record rawGRPCRecord) RenderRaw(sequence uint64, timestamp string) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString("--- ARIES GRPC CALL BEGIN ---\n")
	bridgekit.WriteRawField(&output, "sequence", fmt.Sprint(sequence))
	bridgekit.WriteRawField(&output, "sandbox_id", record.SandboxID)
	bridgekit.WriteRawField(&output, "timestamp", timestamp)
	bridgekit.WriteRawField(&output, "operation_class", record.OperationClass)
	bridgekit.WriteRawField(&output, "status", record.Status)
	bridgekit.WriteRawField(&output, "run_id", record.RunID)
	bridgekit.WriteRawField(&output, "task_id", record.TaskID)
	bridgekit.WriteRawField(&output, "container_id", record.ContainerID)
	bridgekit.WriteRawField(&output, "path", record.Path)
	bridgekit.WriteRawField(&output, "login", fmt.Sprint(record.Login))
	bridgekit.WriteRawField(&output, "script", record.Script)
	bridgekit.WriteRawField(&output, "stdin_bytes", fmt.Sprint(record.StdinBytes))
	bridgekit.WriteRawBytesField(&output, "stdin", record.Stdin)
	bridgekit.WriteRawField(&output, "read_bytes", fmt.Sprint(record.ReadBytes))
	bridgekit.WriteRawField(&output, "sha256", record.SHA256)
	output.WriteString("--- ARIES GRPC CALL END ---\n")
	return output.Bytes(), nil
}

// describeInput renders input for tool-calls.jsonl. content holds the
// retained bytes; it is shorter than count when the input exceeded the
// retained bound and was dropped.
func (session *bridgeSession) describeInput(content []byte, count int64) (string, string) {
	if int64(len(content)) < count {
		return sizeOmitted(count), "size-omitted"
	}
	if bridgekit.SafeStructuredText(content) {
		return string(content), "utf-8"
	}
	// Without the raw log the bytes are retained nowhere, so the note must not
	// point at an artifact this run did not write.
	if session.audit.RetainsRaw() {
		return fmt.Sprintf("[binary input omitted; %d bytes retained in grpc_raw.log]", count), "binary-omitted"
	}
	return fmt.Sprintf("[binary input omitted; %d bytes not retained]", count), "binary-omitted"
}

func sizeOmitted(count int64) string {
	return fmt.Sprintf("[input omitted; %d bytes exceed the retained bound]", count)
}

// boundedWriter retains at most limit bytes while reporting everything the
// sandbox produced, so a truncated call still records its true output volume.
type boundedWriter struct {
	writer io.Writer
	limit  int64

	mu      sync.Mutex
	total   int64
	written int64
	cut     bool
}

func newBoundedWriter(writer io.Writer, limit int64) *boundedWriter {
	return &boundedWriter{writer: writer, limit: limit}
}

func (writer *boundedWriter) Write(content []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	offered := len(content)
	writer.total += int64(offered)
	remaining := writer.limit - writer.written
	if remaining <= 0 {
		writer.cut = true
		return offered, nil
	}
	if int64(offered) > remaining {
		content = content[:remaining]
		writer.cut = true
	}
	n, err := writer.writer.Write(content)
	writer.written += int64(n)
	if err != nil {
		return n, err
	}
	// Report every offered byte as accepted. A short write would look to the
	// sandbox like a failed copy, when the only thing that happened is that
	// ARIES stopped retaining output.
	return offered, nil
}

func (writer *boundedWriter) count() int64 {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.total
}

func (writer *boundedWriter) truncated() bool {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.cut
}
