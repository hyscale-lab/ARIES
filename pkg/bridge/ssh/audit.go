package ssh

import (
	"bytes"
	"context"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
)

// toolCallRecord is the SSH line of tool-calls.jsonl. request_type and
// want_reply stay the last two keys.
type toolCallRecord struct {
	bridgekit.ToolCallRecord
	RequestType string `json:"request_type"`
	WantReply   bool   `json:"want_reply"`
}

type rawSSHRecord struct {
	SandboxID    string
	Sequence     uint64
	Timestamp    string
	RequestType  string
	WantReply    bool
	Status       string
	RunID        string
	TaskID       string
	ContainerID  string
	WireCommand  string
	Payload      []byte
	PayloadBytes int64
	Stdin        []byte
	StdinBytes   int64
}

type requestAudit struct {
	requestType   string
	wantReply     bool
	payload       []byte
	remoteCommand string
}

func (record rawSSHRecord) RenderRaw(sequence uint64, timestamp string) ([]byte, error) {
	record.Sequence, record.Timestamp = sequence, timestamp
	return renderRawSSHRecord(record)
}

func (session *bridgeSession) closeAudit(ctx context.Context) error {
	if session.audit == nil {
		return nil
	}
	return session.audit.SealAndWait(ctx)
}

func renderRawSSHRecord(record rawSSHRecord) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString("--- ARIES SSH CALL BEGIN ---\n")
	bridgekit.WriteRawField(&output, "sequence", fmt.Sprint(record.Sequence))
	bridgekit.WriteRawField(&output, "sandbox_id", record.SandboxID)
	bridgekit.WriteRawField(&output, "timestamp", record.Timestamp)
	bridgekit.WriteRawField(&output, "request_type", record.RequestType)
	bridgekit.WriteRawField(&output, "want_reply", fmt.Sprint(record.WantReply))
	bridgekit.WriteRawField(&output, "status", record.Status)
	bridgekit.WriteRawField(&output, "run_id", record.RunID)
	bridgekit.WriteRawField(&output, "task_id", record.TaskID)
	bridgekit.WriteRawField(&output, "container_id", record.ContainerID)
	bridgekit.WriteRawField(&output, "wire_command", record.WireCommand)
	bridgekit.WriteRawField(&output, "payload_bytes", fmt.Sprint(record.PayloadBytes))
	bridgekit.WriteRawBytesField(&output, "payload", record.Payload)
	bridgekit.WriteRawField(&output, "stdin_bytes", fmt.Sprint(record.StdinBytes))
	bridgekit.WriteRawBytesField(&output, "stdin", record.Stdin)
	output.WriteString("--- ARIES SSH CALL END ---\n")
	return output.Bytes(), nil
}
