package ssh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type toolCallRecord struct {
	SandboxID      string   `json:"sandbox_id"`
	Sequence       uint64   `json:"sequence"`
	Timestamp      string   `json:"timestamp"`
	ContainerID    string   `json:"container_id"`
	ContainerName  string   `json:"container_name"`
	OperationClass string   `json:"operation_class"`
	WorkspaceHome  string   `json:"workspace_home,omitempty"`
	Environment    []string `json:"env_names,omitempty"`
	Path           string   `json:"path,omitempty"`
	Workdir        string   `json:"workdir,omitempty"`
	CommandHash    string   `json:"command_hash"`
	Command        string   `json:"command,omitempty"`
	Argv           []string `json:"argv,omitempty"`
	Stdin          string   `json:"stdin"`
	StdinEncoding  string   `json:"stdin_encoding"`
	StdinBytes     int64    `json:"stdin_bytes"`
	StdoutBytes    int64    `json:"stdout_bytes"`
	StderrBytes    int64    `json:"stderr_bytes"`
	ExitCode       int      `json:"exit_code"`
	DurationMS     int64    `json:"duration_ms"`
	Status         string   `json:"status"`
	Error          string   `json:"error,omitempty"`
	RunID          string   `json:"run_id,omitempty"`
	TaskID         string   `json:"task_id,omitempty"`
	RequestType    string   `json:"request_type"`
	WantReply      bool     `json:"want_reply"`
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

type auditFile struct {
	write func([]byte) (int, error)
	sync  func() error
	close func() error
}

type auditEntry struct {
	structured []byte
	raw        []byte
}

type auditWriter struct {
	structured *auditFile
	raw        *auditFile

	mu        sync.Mutex
	pending   []auditEntry
	sequence  uint64
	bytes     int64
	sealed    bool
	err       error
	wake      chan struct{}
	done      chan struct{}
	marshal   func(any) ([]byte, error)
	renderRaw func(rawSSHRecord) ([]byte, error)
	now       func() time.Time
}

func openAuditFile(path string) (*auditFile, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &auditFile{write: file.Write, sync: file.Sync, close: file.Close}, nil
}

func newAuditWriter(structured, raw *auditFile) *auditWriter {
	writer := &auditWriter{
		structured: structured, raw: raw,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		marshal: marshalJSONLine, renderRaw: renderRawSSHRecord, now: time.Now,
	}
	go writer.run()
	return writer
}

// retainsRaw reports whether this run writes ssh_raw.log, so callers can
// describe where omitted bytes were kept without guessing.
func (writer *auditWriter) retainsRaw() bool {
	return writer != nil && writer.raw != nil
}

func (writer *auditWriter) enqueue(structured toolCallRecord, raw rawSSHRecord) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return
	}
	if writer.sealed {
		writer.latchLocked(errors.New("enqueue SSH audit after seal"))
		return
	}
	sequence := writer.sequence + 1
	timestamp := writer.now().UTC().Format(time.RFC3339Nano)
	structured.Sequence, structured.Timestamp = sequence, timestamp
	raw.Sequence, raw.Timestamp = sequence, timestamp
	structuredLine, err := writer.marshal(structured)
	if err != nil {
		writer.latchLocked(fmt.Errorf("marshal structured SSH audit: %w", err))
		return
	}
	var rawLine []byte
	if writer.raw != nil {
		rawLine, err = writer.renderRaw(raw)
		if err != nil {
			writer.latchLocked(fmt.Errorf("render raw SSH audit: %w", err))
			return
		}
	}
	charge := int64(len(structuredLine) + len(rawLine))
	if charge > maxToolLogBytes-writer.bytes {
		writer.latchLocked(fmt.Errorf("SSH combined audit exceeds %d bytes", maxToolLogBytes))
		return
	}
	writer.sequence = sequence
	writer.bytes += charge
	writer.pending = append(writer.pending, auditEntry{structured: structuredLine, raw: rawLine})
	writer.signal()
}

func marshalJSONLine(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeRawField(output *bytes.Buffer, key, value string) {
	writeRawBytesField(output, key, []byte(value))
}

func writeRawBytesField(output *bytes.Buffer, key string, value []byte) {
	output.WriteString(key)
	output.WriteByte('=')
	writeEscapedRaw(output, value)
	output.WriteByte('\n')
}

func writeEscapedRaw(output *bytes.Buffer, value []byte) {
	for len(value) > 0 {
		switch value[0] {
		case '\\':
			output.WriteString(`\\`)
			value = value[1:]
			continue
		case '\n':
			output.WriteString(`\n`)
			value = value[1:]
			continue
		case '\r':
			output.WriteString(`\r`)
			value = value[1:]
			continue
		case '\t':
			output.WriteString(`\t`)
			value = value[1:]
			continue
		}
		runeValue, size := utf8.DecodeRune(value)
		if runeValue != utf8.RuneError || size > 1 {
			if unicode.IsPrint(runeValue) {
				output.Write(value[:size])
			} else {
				writeHexEscapes(output, value[:size])
			}
			value = value[size:]
			continue
		}
		writeHexEscapes(output, value[:1])
		value = value[1:]
	}
}

func writeHexEscapes(output *bytes.Buffer, value []byte) {
	const uppercaseHex = "0123456789ABCDEF"
	for _, item := range value {
		output.WriteString(`\x`)
		output.WriteByte(uppercaseHex[item>>4])
		output.WriteByte(uppercaseHex[item&0x0f])
	}
}

func (writer *auditWriter) signal() {
	select {
	case writer.wake <- struct{}{}:
	default:
	}
}

func (writer *auditWriter) latchLocked(err error) {
	writer.err = errors.Join(writer.err, err)
}

func (writer *auditWriter) latch(err error) {
	writer.mu.Lock()
	writer.latchLocked(err)
	writer.mu.Unlock()
}

func (writer *auditWriter) run() {
	defer close(writer.done)
	for {
		<-writer.wake
		for {
			writer.mu.Lock()
			if len(writer.pending) == 0 {
				sealed := writer.sealed
				writer.mu.Unlock()
				if sealed {
					writer.finish()
					return
				}
				break
			}
			entry := writer.pending[0]
			writer.pending[0] = auditEntry{}
			writer.pending = writer.pending[1:]
			writer.mu.Unlock()
			writer.persist(entry)
		}
	}
}

func (writer *auditWriter) persist(entry auditEntry) {
	writer.persistLine(writer.structured, entry.structured, "structured write")
	writer.persistLine(writer.raw, entry.raw, "raw write")
	writer.persistSync(writer.structured, "structured sync")
	writer.persistSync(writer.raw, "raw sync")
}

func (writer *auditWriter) persistLine(file *auditFile, line []byte, operation string) {
	if file == nil {
		return
	}
	written, err := file.write(line)
	if err == nil && written != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		writer.mu.Lock()
		writer.latchLocked(fmt.Errorf("%s: %w", operation, err))
		writer.mu.Unlock()
	}
}

func (writer *auditWriter) persistSync(file *auditFile, operation string) {
	if file == nil {
		return
	}
	if err := file.sync(); err != nil {
		writer.mu.Lock()
		writer.latchLocked(fmt.Errorf("%s: %w", operation, err))
		writer.mu.Unlock()
	}
}

func (writer *auditWriter) finish() {
	writer.persistSync(writer.structured, "final structured sync")
	writer.persistSync(writer.raw, "final raw sync")
	for _, item := range []struct {
		name string
		file *auditFile
	}{{"structured close", writer.structured}, {"raw close", writer.raw}} {
		if item.file == nil {
			continue
		}
		if err := item.file.close(); err != nil {
			writer.mu.Lock()
			writer.latchLocked(fmt.Errorf("%s: %w", item.name, err))
			writer.mu.Unlock()
		}
	}
}

func (writer *auditWriter) sealAndWait(ctx context.Context) error {
	if writer == nil {
		return nil
	}
	writer.mu.Lock()
	writer.sealed = true
	writer.signal()
	writer.mu.Unlock()
	select {
	case <-writer.done:
		writer.mu.Lock()
		defer writer.mu.Unlock()
		return writer.err
	case <-ctx.Done():
		return fmt.Errorf("drain SSH audit: %w", ctx.Err())
	}
}

func (writer *auditWriter) finished() bool {
	if writer == nil {
		return true
	}
	select {
	case <-writer.done:
		return true
	default:
		return false
	}
}

func (session *bridgeSession) closeAudit(ctx context.Context) error {
	if session.audit == nil {
		return nil
	}
	return session.audit.sealAndWait(ctx)
}

func renderRawSSHRecord(record rawSSHRecord) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString("--- ARIES SSH CALL BEGIN ---\n")
	writeRawField(&output, "sequence", fmt.Sprint(record.Sequence))
	writeRawField(&output, "sandbox_id", record.SandboxID)
	writeRawField(&output, "timestamp", record.Timestamp)
	writeRawField(&output, "request_type", record.RequestType)
	writeRawField(&output, "want_reply", fmt.Sprint(record.WantReply))
	writeRawField(&output, "status", record.Status)
	writeRawField(&output, "run_id", record.RunID)
	writeRawField(&output, "task_id", record.TaskID)
	writeRawField(&output, "container_id", record.ContainerID)
	writeRawField(&output, "wire_command", record.WireCommand)
	writeRawField(&output, "payload_bytes", fmt.Sprint(record.PayloadBytes))
	writeRawBytesField(&output, "payload", record.Payload)
	writeRawField(&output, "stdin_bytes", fmt.Sprint(record.StdinBytes))
	writeRawBytesField(&output, "stdin", record.Stdin)
	output.WriteString("--- ARIES SSH CALL END ---\n")
	return output.Bytes(), nil
}
