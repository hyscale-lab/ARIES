package hermesgrpc

import (
	"encoding/base64"
	"fmt"
	"io"
	"sync"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
)

// This bridge writes one artifact, tool-calls.jsonl, where the SSH bridges
// also keep ssh_raw.log for three things their structured record cannot hold.
// Each is accounted for here rather than assumed away:
//
//   - The verbatim wire command. Accepted calls need nothing: hermeswire
//     accepts only the canonical encoding, so the recorded command already is
//     the payload that arrived. Refused calls have no canonical encoding, so
//     Command is recorded for those too.
//   - Binary stdin. JSON cannot carry arbitrary bytes, so StdinRaw holds them
//     base64-encoded when they are not structured-safe.
//   - The SSH request framing. This has no successor and is the one accepted
//     loss; a protobuf request is not a comparable artifact.

// toolCallRecord is one line of tool-calls.jsonl. The SSH record's
// request_type and want_reply fields have no analogue here; operation_class
// carries the method name instead.
type toolCallRecord struct {
	bridgekit.Stamp
	ContainerID    string   `json:"container_id"`
	ContainerName  string   `json:"container_name"`
	OperationClass string   `json:"operation_class"`
	Path           string   `json:"path,omitempty"`
	Workdir        string   `json:"workdir,omitempty"`
	CommandHash    string   `json:"command_hash"`
	Command        string   `json:"command,omitempty"`
	Argv           []string `json:"argv,omitempty"`
	Stdin          string   `json:"stdin"`
	StdinEncoding  string   `json:"stdin_encoding"`
	// StdinRaw carries base64 bytes only when Stdin holds the omission note,
	// so a structured-safe input is never stored twice.
	StdinRaw string `json:"stdin_raw,omitempty"`
	// ContentRaw carries a file procedure's content, base64, only when the
	// profile set bridge.retain_raw_log.
	ContentRaw string `json:"content_raw,omitempty"`
	// SHA256 is the hex digest of a completed file procedure's content.
	SHA256      string `json:"sha256,omitempty"`
	StdinBytes  int64  `json:"stdin_bytes"`
	StdoutBytes int64  `json:"stdout_bytes"`
	Truncated   bool   `json:"truncated,omitempty"`
	StderrBytes int64  `json:"stderr_bytes"`
	ExitCode    int    `json:"exit_code"`
	DurationMS  int64  `json:"duration_ms"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
}

// boundedWriter retains at most limit bytes while reporting everything the
// sandbox produced, so a truncated call still records its true output volume.
// It replaces the SSH bridge's byteCounter, which needed no bound because it
// wrote straight to the channel instead of buffering a reply.
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

// describeStdin renders retained input for the record. JSON cannot hold
// arbitrary bytes, so anything not structured-safe goes to stdin_raw as base64
// and the Stdin field carries a note naming that field.
func describeStdin(content []byte) (text, encoding, raw string) {
	if bridgekit.SafeStructuredText(content) {
		return string(content), "utf-8", ""
	}
	return fmt.Sprintf("[binary input omitted; %d bytes retained in stdin_raw]", len(content)),
		"binary-omitted", base64.StdEncoding.EncodeToString(content)
}
