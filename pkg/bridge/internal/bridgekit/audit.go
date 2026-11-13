// Package bridgekit holds what every tool bridge transport shares: the audit
// writer behind tool-calls.jsonl and the raw log, the record format, and the
// private artifact directory.
package bridgekit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const (
	MaxRecordedInputBytes = 16 << 20
	maxToolLogBytes       = 256 << 20
)

// ToolCallRecord holds the fields every transport records.
type ToolCallRecord struct {
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
	StartedAt      string   `json:"started_at"`
	DurationMS     int64    `json:"duration_ms"`
	Status         string   `json:"status"`
	Error          string   `json:"error,omitempty"`
	RunID          string   `json:"run_id,omitempty"`
	TaskID         string   `json:"task_id,omitempty"`
	ToolCallID     string   `json:"tool_call_id,omitempty"`
}

// FormatTime gives started_at and timestamp one format.
func FormatTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// Stamp is promoted to each transport's record, which embeds ToolCallRecord
// first and adds its own fields after it.
func (record *ToolCallRecord) Stamp(sequence uint64, timestamp string) {
	record.Sequence, record.Timestamp = sequence, timestamp
}

// Record is one line of tool-calls.jsonl. The writer stamps it under its lock.
type Record interface {
	Stamp(sequence uint64, timestamp string)
}

// RawRecord is one entry of a transport's raw log. The writer stamps it under
// its lock, so the raw entry and the structured record share one sequence.
type RawRecord interface {
	RenderRaw(sequence uint64, timestamp string) ([]byte, error)
}

type AuditFile struct {
	Write func([]byte) (int, error)
	Sync  func() error
	Close func() error
}

type auditEntry struct {
	structured []byte
	raw        []byte
}

type AuditWriter struct {
	structured *AuditFile
	raw        *AuditFile

	mu       sync.Mutex
	pending  []auditEntry
	sequence uint64
	bytes    int64
	sealed   bool
	err      error
	wake     chan struct{}
	done     chan struct{}
	marshal  func(any) ([]byte, error)
	now      func() time.Time
}

func OpenAuditFile(path string) (*AuditFile, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &AuditFile{Write: file.Write, Sync: file.Sync, Close: file.Close}, nil
}

func NewAuditWriter(structured, raw *AuditFile) *AuditWriter {
	writer := &AuditWriter{
		structured: structured, raw: raw,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		marshal: marshalJSONLine, now: time.Now,
	}
	go writer.run()
	return writer
}

// RetainsRaw reports whether this run writes the raw log, so callers can
// describe where omitted bytes were kept without guessing.
func (writer *AuditWriter) RetainsRaw() bool {
	return writer != nil && writer.raw != nil
}

func (writer *AuditWriter) Enqueue(structured Record, raw RawRecord) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return
	}
	if writer.sealed {
		writer.latchLocked(errors.New("enqueue audit after seal"))
		return
	}
	sequence := writer.sequence + 1
	timestamp := FormatTime(writer.now())
	structured.Stamp(sequence, timestamp)
	structuredLine, err := writer.marshal(structured)
	if err != nil {
		writer.latchLocked(fmt.Errorf("marshal structured audit: %w", err))
		return
	}
	var rawLine []byte
	if writer.raw != nil {
		rawLine, err = raw.RenderRaw(sequence, timestamp)
		if err != nil {
			writer.latchLocked(fmt.Errorf("render raw audit: %w", err))
			return
		}
	}
	charge := int64(len(structuredLine) + len(rawLine))
	if charge > maxToolLogBytes-writer.bytes {
		writer.latchLocked(fmt.Errorf("combined audit exceeds %d bytes", maxToolLogBytes))
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

func WriteRawField(output *bytes.Buffer, key, value string) {
	WriteRawBytesField(output, key, []byte(value))
}

func WriteRawBytesField(output *bytes.Buffer, key string, value []byte) {
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

// Err reports the latched error. After an error the writer drops each record,
// so a transport must refuse new sandbox calls.
func (writer *AuditWriter) Err() error {
	if writer == nil {
		return nil
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.err
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
		return fmt.Errorf("drain audit: %w", ctx.Err())
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

func SafeStructuredText(content []byte) bool {
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

func CommandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}
