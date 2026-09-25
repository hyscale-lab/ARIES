package hermesgrpc

// The evidence machinery below is a deliberate copy of
// pkg/bridge/hermesssh/bridge.go:107-528, minus the raw-log half. Duplication
// is the repository's stated default until a shared seam is proven, and the
// gRPC record shape is still moving, so extracting now would mean designing
// the abstraction around SSH's raw record. Note this is the third copy:
// hermesssh is already a near-verbatim copy of openclawssh.
//
// What is dropped relative to those two: rawSSHRecord and its renderer. This
// bridge writes one artifact, not two. The SSH bridges keep a second
// byte-level log for three things the structured record cannot hold, and each
// is accounted for here rather than assumed away:
//
//   - The verbatim wire command. Accepted calls need nothing: the canonical
//     round-trip check in decodeShellToken means the recorded command already
//     is the payload that arrived. Refused calls have no canonical encoding by
//     definition, so Command is recorded for those too — the SSH bridge stores
//     only a hash there and keeps the bytes in the raw log alone.
//   - Binary stdin. JSON cannot carry arbitrary bytes, which is a property of
//     the file format rather than of SSH, so StdinRaw holds them base64-encoded
//     when they are not structured-safe.
//   - The SSH request framing. This has no successor and is the one accepted
//     loss; a protobuf request is not a comparable artifact.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
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
	// StdinRaw carries base64 bytes only when Stdin holds the omission note,
	// so a structured-safe input is never stored twice.
	StdinRaw string `json:"stdin_raw,omitempty"`
	// ContentRaw carries a file procedure's content, base64, only when the
	// profile set bridge.retain_raw_log.
	ContentRaw  string `json:"content_raw,omitempty"`
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
	if safeStructuredText(content) {
		return string(content), "utf-8", ""
	}
	return fmt.Sprintf("[binary input omitted; %d bytes retained in stdin_raw]", len(content)),
		"binary-omitted", base64.StdEncoding.EncodeToString(content)
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

// stageExecutable copies the client binary into the task artifact directory,
// verifying the source did not change underneath the read.
func stageExecutable(source, destination string) error {
	before, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 {
		return errors.New("helper source must be a regular executable")
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	after, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != int64(len(content)) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("helper source changed while being staged")
	}
	return writeExclusive(destination, content, 0o555)
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
