package hermesgrpc

// The evidence machinery below is a deliberate copy of
// pkg/bridge/hermesssh/bridge.go:107-528, minus the raw-log half. Duplication
// is the repository's stated default until a shared seam is proven, and the
// gRPC record shape is still moving, so extracting now would mean designing
// the abstraction around SSH's raw record. Note this is the third copy:
// hermesssh is already a near-verbatim copy of openclawssh.
//
// What is dropped relative to those two: rawSSHRecord and its renderer. The
// SSH bridges keep a second byte-level artifact because their wire command
// differs from the executed command — quoting and translation mean the
// structured record alone loses fidelity. Here the request message carries the
// script verbatim, so there is nothing the structured record cannot express.
// The one thing lost is binary stdin retention, which is now noted rather than
// kept.

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
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// toolCallRecord is one line of tool-calls.jsonl. The SSH record's
// request_type and want_reply fields have no analogue here; operation_class
// carries the method name instead.
type toolCallRecord struct {
	Sequence       uint64   `json:"sequence"`
	Timestamp      string   `json:"timestamp"`
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
	StdinBytes     int64    `json:"stdin_bytes"`
	StdoutBytes    int64    `json:"stdout_bytes"`
	StderrBytes    int64    `json:"stderr_bytes"`
	ExitCode       int      `json:"exit_code"`
	DurationMS     int64    `json:"duration_ms"`
	Status         string   `json:"status"`
	Error          string   `json:"error,omitempty"`
	RunID          string   `json:"run_id,omitempty"`
	TaskID         string   `json:"task_id,omitempty"`
}

type auditFile struct {
	write func([]byte) (int, error)
	sync  func() error
	close func() error
}

// auditWriter is one bounded asynchronous writer. Handler paths enqueue
// complete immutable records rather than performing storage I/O, and every
// failure latches so that revocation cannot be confirmed on an incomplete
// record.
type auditWriter struct {
	structured *auditFile

	mu       sync.Mutex
	pending  [][]byte
	sequence uint64
	bytes    int64
	sealed   bool
	err      error
	wake     chan struct{}
	done     chan struct{}
	marshal  func(any) ([]byte, error)
	now      func() time.Time
}

type byteCounter struct {
	writer io.Writer
	n      atomic.Int64
}

func (counter *byteCounter) Write(content []byte) (int, error) {
	n, err := counter.writer.Write(content)
	counter.n.Add(int64(n))
	return n, err
}

func (counter *byteCounter) count() int64 { return counter.n.Load() }

// recordedInput taps stdin for the audit while it streams to the sandbox.
// Exceeding the bound discards what was buffered and reports the overflow, so
// a partial record is never written.
type recordedInput struct {
	reader   io.Reader
	mu       sync.Mutex
	n        int64
	data     bytes.Buffer
	overflow bool
}

func (input *recordedInput) Read(content []byte) (int, error) {
	n, err := input.reader.Read(content)
	if n > 0 {
		input.mu.Lock()
		remaining := maxRecordedInputBytes - input.data.Len()
		if n > remaining {
			input.n += int64(n)
			input.data.Reset()
			input.overflow = true
			input.mu.Unlock()
			return n, fmt.Errorf("Hermes gRPC stdin exceeds %d bytes", maxRecordedInputBytes)
		}
		_, _ = input.data.Write(content[:n])
		input.n += int64(n)
		input.mu.Unlock()
	}
	return n, err
}

func (input *recordedInput) record() (int64, string, string, bool) {
	input.mu.Lock()
	count := input.n
	content := bytes.Clone(input.data.Bytes())
	overflow := input.overflow
	input.mu.Unlock()
	if safeStructuredText(content) {
		return count, string(content), "utf-8", overflow
	}
	// This bridge writes no raw artifact, so binary input is retained nowhere
	// and the note must not point at a file that does not exist.
	return count, fmt.Sprintf("[binary input omitted; %d bytes not retained]", count), "binary-omitted", overflow
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

func openAuditFile(path string) (*auditFile, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &auditFile{write: file.Write, sync: file.Sync, close: file.Close}, nil
}

func newAuditWriter(structured *auditFile) *auditWriter {
	writer := &auditWriter{
		structured: structured,
		wake:       make(chan struct{}, 1), done: make(chan struct{}),
		marshal: marshalJSONLine, now: time.Now,
	}
	go writer.run()
	return writer
}

func (writer *auditWriter) enqueue(structured toolCallRecord) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return
	}
	if writer.sealed {
		writer.latchLocked(errors.New("enqueue Hermes gRPC audit after seal"))
		return
	}
	sequence := writer.sequence + 1
	structured.Sequence = sequence
	structured.Timestamp = writer.now().UTC().Format(time.RFC3339Nano)
	line, err := writer.marshal(structured)
	if err != nil {
		writer.latchLocked(fmt.Errorf("marshal structured gRPC audit: %w", err))
		return
	}
	charge := int64(len(line))
	if charge > maxToolLogBytes-writer.bytes {
		writer.latchLocked(fmt.Errorf("Hermes gRPC audit exceeds %d bytes", maxToolLogBytes))
		return
	}
	writer.sequence = sequence
	writer.bytes += charge
	writer.pending = append(writer.pending, line)
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
			line := writer.pending[0]
			writer.pending[0] = nil
			writer.pending = writer.pending[1:]
			writer.mu.Unlock()
			writer.persistLine(line, "structured write")
			writer.persistSync("structured sync")
		}
	}
}

func (writer *auditWriter) persistLine(line []byte, operation string) {
	if writer.structured == nil {
		return
	}
	written, err := writer.structured.write(line)
	if err == nil && written != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		writer.latch(fmt.Errorf("%s: %w", operation, err))
	}
}

func (writer *auditWriter) persistSync(operation string) {
	if writer.structured == nil {
		return
	}
	if err := writer.structured.sync(); err != nil {
		writer.latch(fmt.Errorf("%s: %w", operation, err))
	}
}

func (writer *auditWriter) finish() {
	writer.persistSync("final structured sync")
	if writer.structured == nil {
		return
	}
	if err := writer.structured.close(); err != nil {
		writer.latch(fmt.Errorf("structured close: %w", err))
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
		return fmt.Errorf("drain Hermes gRPC audit: %w", ctx.Err())
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

func commandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}

// hasCancellationCause and isPureCancellation keep Stop failing closed: a
// sandbox error returned after revocation is ambiguous unless it carries the
// cancellation cause, so anything that is not provably pure cancellation is
// preserved and fails revocation.
func hasCancellationCause(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isPureCancellation(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isPureCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isPureCancellation(wrapped.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

func writeExclusivePrivate(path string, content []byte) error {
	return writeExclusive(path, content, 0o600)
}

type exclusiveWriteFile interface {
	Write([]byte) (int, error)
	Sync() error
	Chmod(os.FileMode) error
	Close() error
}

type exclusiveWriteOperations struct {
	open   func(string, int, os.FileMode) (exclusiveWriteFile, error)
	remove func(string) error
}

func writeExclusive(path string, content []byte, mode os.FileMode) error {
	return writeExclusiveWithOperations(path, content, mode, exclusiveWriteOperations{
		open: func(path string, flags int, mode os.FileMode) (exclusiveWriteFile, error) {
			return os.OpenFile(path, flags, mode)
		},
		remove: os.Remove,
	})
}

func writeExclusiveWithOperations(path string, content []byte, mode os.FileMode, operations exclusiveWriteOperations) error {
	file, err := operations.open(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := func(primary error, needsClose bool) error {
		var closeErr error
		if needsClose {
			if err := file.Close(); err != nil {
				closeErr = fmt.Errorf("close exclusive file: %w", err)
			}
		}
		removeErr := operations.remove(path)
		if removeErr != nil {
			removeErr = fmt.Errorf("remove failed exclusive file: %w", removeErr)
		}
		return errors.Join(primary, closeErr, removeErr)
	}
	written, err := file.Write(content)
	if err == nil && written != len(content) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return cleanup(fmt.Errorf("write exclusive file: %w", err), true)
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync exclusive file data: %w", err), true)
	}
	if err := file.Chmod(mode); err != nil {
		return cleanup(fmt.Errorf("chmod exclusive file: %w", err), true)
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync exclusive file metadata: %w", err), true)
	}
	if err := file.Close(); err != nil {
		return cleanup(fmt.Errorf("close exclusive file: %w", err), false)
	}
	return nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if resolved != absolute {
		return errors.New("directory path contains a symbolic link")
	}
	return os.Chmod(path, 0o700)
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
