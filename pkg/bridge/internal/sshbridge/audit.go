package sshbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxRecordedInputBytes = 16 << 20
	MaxToolLogBytes       = 256 << 20
)

// ToolCallRecord is the existing JSONL schema shared by the three SSH bridges.
// OpenClaw alone supplies WorkspaceHome and Environment; both remain optional.
type ToolCallRecord struct {
	Sequence       uint64   `json:"sequence"`
	Timestamp      string   `json:"timestamp"`
	ContainerID    string   `json:"container_id"`
	ContainerName  string   `json:"container_name"`
	OperationClass string   `json:"operation_class"`
	Path           string   `json:"path,omitempty"`
	Workdir        string   `json:"workdir,omitempty"`
	WorkspaceHome  string   `json:"workspace_home,omitempty"`
	Environment    []string `json:"env_names,omitempty"`
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

// RawRecord retains exact SSH request and stdin bytes in the private raw log.
type RawRecord struct {
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

// AuditFile holds the concrete operations of one private audit file.
type AuditFile struct {
	Write func([]byte) (int, error)
	Sync  func() error
	Close func() error
}

type auditEntry struct {
	structured []byte
	raw        []byte
}

// AuditWriter persists correlated structured and raw records asynchronously.
// Admission and persistence failures remain latched through every drain attempt.
type AuditWriter struct {
	label      string
	structured *AuditFile
	raw        *AuditFile

	mu        sync.Mutex
	pending   []auditEntry
	sequence  uint64
	bytes     int64
	sealed    bool
	err       error
	wake      chan struct{}
	done      chan struct{}
	marshal   func(any) ([]byte, error)
	renderRaw func(RawRecord) ([]byte, error)
	now       func() time.Time
}

// ByteCounter counts streamed bytes without retaining their contents.
type ByteCounter struct {
	Reader io.Reader
	Writer io.Writer
	n      atomic.Int64
}

func (counter *ByteCounter) Read(content []byte) (int, error) {
	n, err := counter.Reader.Read(content)
	counter.n.Add(int64(n))
	return n, err
}

func (counter *ByteCounter) Write(content []byte) (int, error) {
	n, err := counter.Writer.Write(content)
	counter.n.Add(int64(n))
	return n, err
}

func (counter *ByteCounter) Count() int64 { return counter.n.Load() }

// RecordedInput retains a bounded, synchronized snapshot of task stdin.
type RecordedInput struct {
	label    string
	reader   io.Reader
	mu       sync.Mutex
	n        int64
	data     bytes.Buffer
	overflow bool
}

// NewRecordedInput records only the caller's task input. Private protocol
// prefixes must be composed outside this reader so they never enter artifacts.
func NewRecordedInput(label string, reader io.Reader) *RecordedInput {
	return &RecordedInput{label: label, reader: reader}
}

func (input *RecordedInput) Read(content []byte) (int, error) {
	n, err := input.reader.Read(content)
	if n > 0 {
		input.mu.Lock()
		remaining := MaxRecordedInputBytes - input.data.Len()
		if n > remaining {
			input.n += int64(n)
			input.data.Reset()
			input.overflow = true
			input.mu.Unlock()
			return n, fmt.Errorf("%s SSH stdin exceeds %d bytes", input.label, MaxRecordedInputBytes)
		}
		_, _ = input.data.Write(content[:n])
		input.n += int64(n)
		input.mu.Unlock()
	}
	return n, err
}

func (input *RecordedInput) Record(retainedRaw bool) (int64, string, string, []byte, bool) {
	input.mu.Lock()
	count := input.n
	content := bytes.Clone(input.data.Bytes())
	overflow := input.overflow
	input.mu.Unlock()
	if safeStructuredText(content) {
		return count, string(content), "utf-8", content, overflow
	}
	// Without the raw log the bytes are retained nowhere, so the note must not
	// point at an artifact this run did not write.
	note := fmt.Sprintf("[binary input omitted; %d bytes not retained]", count)
	if retainedRaw {
		note = fmt.Sprintf("[binary input omitted; %d bytes retained in ssh_raw.log]", count)
	}
	return count, note, "binary-omitted", content, overflow
}

func safeStructuredText(content []byte) bool {
	if !utf8.Valid(content) {
		return false
	}
	for _, value := range string(content) {
		if unicode.IsControl(value) && value != '\t' && value != '\n' && value != '\r' {
			return false
		}
	}
	return true
}

// OpenAuditFile creates one private file without following or replacing an
// existing path. The owning bridge prepares its private parent directory.
func OpenAuditFile(path string) (*AuditFile, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &AuditFile{Write: file.Write, Sync: file.Sync, Close: file.Close}, nil
}

// NewAuditWriter starts persistence for an already-open structured file and
// optional raw file. Label is the bridge name used in existing error messages.
func NewAuditWriter(label string, structured, raw *AuditFile) *AuditWriter {
	writer := &AuditWriter{
		label:      label,
		structured: structured, raw: raw,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		marshal: marshalJSONLine, renderRaw: renderRawSSHRecord, now: time.Now,
	}
	go writer.run()
	return writer
}

// RetainsRaw reports whether this run writes ssh_raw.log, so callers can
// describe where omitted bytes were kept without guessing.
func (writer *AuditWriter) RetainsRaw() bool {
	return writer != nil && writer.raw != nil
}

func (writer *AuditWriter) Enqueue(structured ToolCallRecord, raw RawRecord) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return
	}
	if writer.sealed {
		writer.latchLocked(fmt.Errorf("enqueue %s SSH audit after seal", writer.label))
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
	if charge > MaxToolLogBytes-writer.bytes {
		writer.latchLocked(fmt.Errorf("%s SSH combined audit exceeds %d bytes", writer.label, MaxToolLogBytes))
		return
	}
	writer.sequence = sequence
	writer.bytes += charge
	writer.pending = append(writer.pending, auditEntry{
		structured: structuredLine, raw: rawLine,
	})
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

func renderRawSSHRecord(record RawRecord) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString("--- ARIES SSH CALL BEGIN ---\n")
	writeRawField(&output, "sequence", fmt.Sprint(record.Sequence))
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

func (writer *AuditWriter) signal() {
	select {
	case writer.wake <- struct{}{}:
	default:
	}
}

func (writer *AuditWriter) latchLocked(err error) {
	writer.err = errors.Join(writer.err, err)
}

func (writer *AuditWriter) Latch(err error) {
	writer.mu.Lock()
	writer.latchLocked(err)
	writer.mu.Unlock()
}

func (writer *AuditWriter) run() {
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

func (writer *AuditWriter) persist(entry auditEntry) {
	writer.persistLine(writer.structured, entry.structured, "structured write")
	writer.persistLine(writer.raw, entry.raw, "raw write")
	writer.persistSync(writer.structured, "structured sync")
	writer.persistSync(writer.raw, "raw sync")
}

func (writer *AuditWriter) persistLine(file *AuditFile, line []byte, operation string) {
	if file == nil {
		return
	}
	written, err := file.Write(line)
	if err == nil && written != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		writer.mu.Lock()
		writer.latchLocked(fmt.Errorf("%s: %w", operation, err))
		writer.mu.Unlock()
	}
}

func (writer *AuditWriter) persistSync(file *AuditFile, operation string) {
	if file == nil {
		return
	}
	if err := file.Sync(); err != nil {
		writer.mu.Lock()
		writer.latchLocked(fmt.Errorf("%s: %w", operation, err))
		writer.mu.Unlock()
	}
}

func (writer *AuditWriter) finish() {
	writer.persistSync(writer.structured, "final structured sync")
	writer.persistSync(writer.raw, "final raw sync")
	for _, item := range []struct {
		name string
		file *AuditFile
	}{{"structured close", writer.structured}, {"raw close", writer.raw}} {
		if item.file == nil {
			continue
		}
		if err := item.file.Close(); err != nil {
			writer.mu.Lock()
			writer.latchLocked(fmt.Errorf("%s: %w", item.name, err))
			writer.mu.Unlock()
		}
	}
}

func (writer *AuditWriter) SealAndWait(ctx context.Context) error {
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
		return fmt.Errorf("drain %s SSH audit: %w", writer.label, ctx.Err())
	}
}

func (writer *AuditWriter) Finished() bool {
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
