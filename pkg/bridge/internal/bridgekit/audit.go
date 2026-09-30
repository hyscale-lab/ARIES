// Package bridgekit is the transport-independent machinery every ARIES tool
// bridge shares: the audit writer behind tool-calls.jsonl and ssh_raw.log, the
// private artifact files, and the session lifecycle that confirms revocation
// only once the audit is sealed.
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

// Stamp is the sequence and time the writer assigns to each record. A bridge
// record embeds it, untagged and first, so the JSON keeps `sequence` and
// `timestamp` as its leading keys.
//
// Stamp must never gain a MarshalJSON or MarshalText method: it would be
// promoted and replace the encoding of every record that embeds it.
type Stamp struct {
	Sequence  uint64 `json:"sequence"`
	Timestamp string `json:"timestamp"`
}

func (stamp *Stamp) stamp() *Stamp { return stamp }

// Stamped is a record that embeds Stamp.
type Stamped interface{ stamp() *Stamp }

// RawRecord is one entry of ssh_raw.log, the byte-level record of an SSH
// channel request.
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

type auditFile struct {
	write func([]byte) (int, error)
	sync  func() error
	close func() error
}

type auditEntry struct {
	structured []byte
	raw        []byte
}

// Writer is one bounded asynchronous audit writer. Handler paths enqueue
// complete immutable records rather than performing storage I/O, and every
// failure latches so that revocation cannot be confirmed on an incomplete
// record.
type Writer struct {
	structured *auditFile
	raw        *auditFile

	mu       sync.Mutex
	pending  []auditEntry
	sequence uint64
	bytes    int64
	limit    int64
	sealed   bool
	err      error
	wake     chan struct{}
	done     chan struct{}
	marshal  func(any) ([]byte, error)
	now      func() time.Time
}

// Open creates the structured log and, unless rawLog is empty, the raw log,
// both exclusively. limit bounds their combined size.
func Open(toolLog, rawLog string, limit int64) (*Writer, error) {
	structured, err := openAuditFile(toolLog)
	if err != nil {
		return nil, fmt.Errorf("create tool log: %w", err)
	}
	var raw *auditFile
	if rawLog != "" {
		if raw, err = openAuditFile(rawLog); err != nil {
			return nil, errors.Join(fmt.Errorf("create raw log: %w", err), structured.close())
		}
	}
	return newWriter(structured, raw, limit), nil
}

func openAuditFile(path string) (*auditFile, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &auditFile{write: file.Write, sync: file.Sync, close: file.Close}, nil
}

func newWriter(structured, raw *auditFile, limit int64) *Writer {
	writer := &Writer{
		structured: structured, raw: raw, limit: limit,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		marshal: marshalJSONLine, now: time.Now,
	}
	go writer.run()
	return writer
}

// Enqueue stamps and admits one record, and its raw counterpart when the
// writer keeps a raw log. exempt bytes of the record are not charged against
// the limit: a bridge passes the length of content the profile asked to
// retain without bound.
func (writer *Writer) Enqueue(record Stamped, raw *RawRecord, exempt int) {
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
	timestamp := writer.now().UTC().Format(time.RFC3339Nano)
	stamp := record.stamp()
	stamp.Sequence, stamp.Timestamp = sequence, timestamp
	structuredLine, err := writer.marshal(record)
	if err != nil {
		writer.latchLocked(fmt.Errorf("marshal audit record: %w", err))
		return
	}
	var rawLine []byte
	if writer.raw != nil && raw != nil {
		raw.Sequence, raw.Timestamp = sequence, timestamp
		rawLine = renderRawRecord(*raw)
	}
	charge := int64(len(structuredLine) + len(rawLine) - exempt)
	if charge > writer.limit-writer.bytes {
		writer.latchLocked(fmt.Errorf("audit exceeds %d bytes", writer.limit))
		return
	}
	writer.sequence = sequence
	writer.bytes += charge
	writer.pending = append(writer.pending, auditEntry{structured: structuredLine, raw: rawLine})
	writer.signal()
}

// Latch fails the audit, and therefore revocation, with err.
func (writer *Writer) Latch(err error) {
	writer.mu.Lock()
	writer.latchLocked(err)
	writer.mu.Unlock()
}

func (writer *Writer) latchLocked(err error) {
	writer.err = errors.Join(writer.err, err)
}

func (writer *Writer) signal() {
	select {
	case writer.wake <- struct{}{}:
	default:
	}
}

func (writer *Writer) run() {
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
			writer.persistLine(writer.structured, entry.structured, "structured write")
			writer.persistLine(writer.raw, entry.raw, "raw write")
			writer.persistSync(writer.structured, "structured sync")
			writer.persistSync(writer.raw, "raw sync")
		}
	}
}

func (writer *Writer) persistLine(file *auditFile, line []byte, operation string) {
	if file == nil {
		return
	}
	written, err := file.write(line)
	if err == nil && written != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		writer.Latch(fmt.Errorf("%s: %w", operation, err))
	}
}

func (writer *Writer) persistSync(file *auditFile, operation string) {
	if file == nil {
		return
	}
	if err := file.sync(); err != nil {
		writer.Latch(fmt.Errorf("%s: %w", operation, err))
	}
}

func (writer *Writer) finish() {
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
			writer.Latch(fmt.Errorf("%s: %w", item.name, err))
		}
	}
}

// sealAndWait stops admission and waits for every admitted record to be
// persisted. A timeout leaves the writer draining, so a later call can retry.
func (writer *Writer) sealAndWait(ctx context.Context) error {
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

func (writer *Writer) finished() bool {
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

func marshalJSONLine(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func renderRawRecord(record RawRecord) []byte {
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
	return output.Bytes()
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

// SafeText reports whether content can sit in a JSON string as readable text:
// valid UTF-8 with no control characters other than tab and line breaks.
func SafeText(content []byte) bool {
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

// CommandHash is the recorded digest of a wire command.
func CommandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}
