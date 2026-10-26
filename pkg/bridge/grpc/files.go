package grpc

// The four file procedures. The bridge owns policy here — revocation, the
// absolute-path rule, status mapping and the audit record — and the borrowed
// target owns how bytes land. File content streams in chunks and has no size
// bound: the bridge holds one chunk at a time, plus written content up to the
// retained bound for the audit.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"path"
	"strings"
	"syscall"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// The operation class of a file call is its E2B operation. ReadFile and
	// ReadLines are both a read; the read window tells them apart.
	kindFileStat  = "stat"
	kindFileRead  = "read"
	kindFileWrite = "write"

	// chunkBytes is the content carried by one stream message.
	chunkBytes = 64 << 10
)

// fileCall is one admitted file procedure with a context that both
// revocation and the client going away cancel.
type fileCall struct {
	session *bridgeSession
	kind    string
	path    string
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
	window  fileWindow
	// toolCallID is the harness tool call the file call serves, or "".
	toolCallID string
}

// beginFile admits a file call. invalid, when set, is the reason the request
// arguments are refused; revocation is checked first. A refused call is
// recorded with started at the time of the refusal.
func (svc *service) beginFile(ctx context.Context, kind, filePath, invalid string, window fileWindow) (*fileCall, error) {
	session := svc.session
	toolCallID, validID := incomingToolCallID(ctx)
	call := &fileCall{session: session, kind: kind, path: filePath, started: time.Now(), window: window, toolCallID: toolCallID}
	reject := func(code codes.Code, message string) (*fileCall, error) {
		call.recordFile(nil, false, 0, "rejected", message)
		return nil, status.Error(code, message)
	}
	if session.revoked.Load() {
		return reject(codes.Unavailable, "session revoked")
	}
	if !validID {
		return reject(codes.InvalidArgument, invalidToolCallID)
	}
	if invalid != "" {
		return reject(codes.InvalidArgument, invalid)
	}
	if !path.IsAbs(filePath) || strings.ContainsRune(filePath, 0) {
		return reject(codes.InvalidArgument, "path must be absolute")
	}
	call.ctx, call.cancel = session.callContext(ctx)
	return call, nil
}

// finish records the outcome and converts a target error to a status. A call
// the target completed is recorded as completed even after a cancellation.
func (call *fileCall) finish(err error, sink *contentSink, truncated bool) error {
	defer call.cancel()
	statusText, message := "completed", ""
	var result error
	if err != nil {
		result = fileStatus(err)
		statusText, message = "failed", status.Convert(result).Message()
		if status.Code(result) == codes.Canceled {
			statusText = "canceled"
		}
	}
	call.recordFile(sink, truncated, time.Since(call.started).Milliseconds(), statusText, message)
	return result
}

// streamErr reports a failed stream operation as the call's cancellation when
// the call has ended. The client going away then reads as a cancellation, so
// the record says canceled, not failed.
func (call *fileCall) streamErr(err error) error {
	if err != nil && call.ctx.Err() != nil {
		return call.ctx.Err()
	}
	return err
}

func fileStatus(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return status.Error(codes.NotFound, "no such file")
	case errors.Is(err, fs.ErrPermission):
		return status.Error(codes.PermissionDenied, "permission denied")
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR), errors.Is(err, errNotRegular):
		return status.Error(codes.FailedPrecondition, "not a regular file")
	case errors.Is(err, errWriteStream):
		return status.Error(codes.InvalidArgument, errWriteStream.Error())
	case bridgekit.HasCancellationCause(err):
		return status.Error(codes.Canceled, "session canceled")
	default:
		return status.Error(codes.Internal, "sandbox file access failed")
	}
}

var (
	newlineByte    = []byte{'\n'}
	errNotRegular  = errors.New("not a regular file")
	errWriteStream = errors.New("write stream does not match its header")
)

// contentSink sees every content byte of a file call once: it counts and
// hashes them for the audit, keeps written content up to the retained bound,
// and passes them on to next when there is one.
type contentSink struct {
	next  io.Writer
	hash  hash.Hash
	keep  bool
	kept  bytes.Buffer
	count int64
}

func newSink(next io.Writer, keep bool) *contentSink {
	return &contentSink{next: next, hash: sha256.New(), keep: keep}
}

func (sink *contentSink) Write(content []byte) (int, error) {
	if sink.next != nil {
		if written, err := sink.next.Write(content); err != nil {
			return written, err
		}
	}
	sink.hash.Write(content)
	sink.count += int64(len(content))
	if sink.keep && sink.count <= bridgekit.MaxRecordedInputBytes {
		sink.kept.Write(content)
	} else if sink.keep {
		// Past the bound only the size and digest are recorded and the write
		// continues. This differs from SSH, where stdin past the bound fails the
		// command and latches the audit.
		sink.keep = false
		sink.kept = bytes.Buffer{}
	}
	return len(content), nil
}

// chunkWriter batches content into stream messages of at most chunkBytes.
// Each message gets its own buffer: gRPC may still hold a sent message.
type chunkWriter struct {
	send    func([]byte) error
	pending []byte
}

func (writer *chunkWriter) Write(content []byte) (int, error) {
	total := len(content)
	for len(content) > 0 {
		if writer.pending == nil {
			writer.pending = make([]byte, 0, chunkBytes)
		}
		take := min(chunkBytes-len(writer.pending), len(content))
		writer.pending = append(writer.pending, content[:take]...)
		content = content[take:]
		if len(writer.pending) == chunkBytes {
			if err := writer.flush(); err != nil {
				return total - len(content), err
			}
		}
	}
	return total, nil
}

func (writer *chunkWriter) flush() error {
	if len(writer.pending) == 0 {
		return nil
	}
	err := writer.send(writer.pending)
	writer.pending = nil
	return err
}

func (svc *service) Stat(ctx context.Context, request *sandboxv1.StatRequest) (*sandboxv1.StatResponse, error) {
	call, err := svc.beginFile(ctx, kindFileStat, request.GetPath(), "", fileWindow{})
	if err != nil {
		return nil, err
	}
	info, err := svc.session.sandbox.StatFile(call.ctx, call.path)
	if errors.Is(err, fs.ErrNotExist) {
		return &sandboxv1.StatResponse{}, call.finish(nil, nil, false)
	}
	if err != nil {
		return nil, call.finish(err, nil, false)
	}
	response := &sandboxv1.StatResponse{Exists: true, Mode: uint32(info.Mode().Perm()), Type: sandboxv1.FileType_FILE_TYPE_OTHER}
	switch {
	case info.Mode().IsRegular():
		response.Type, response.Size = sandboxv1.FileType_FILE_TYPE_REGULAR, info.Size()
	case info.IsDir():
		response.Type = sandboxv1.FileType_FILE_TYPE_DIRECTORY
	}
	return response, call.finish(nil, nil, false)
}

func (svc *service) ReadFile(request *sandboxv1.ReadFileRequest, stream grpc.ServerStreamingServer[sandboxv1.ReadFileResponse]) error {
	limit := request.GetMaxBytes()
	invalid := ""
	if limit < 0 {
		invalid = "max_bytes out of range"
	}
	call, err := svc.beginFile(stream.Context(), kindFileRead, request.GetPath(), invalid, fileWindow{MaxBytes: limit})
	if err != nil {
		return err
	}
	chunks := &chunkWriter{send: func(chunk []byte) error {
		return call.streamErr(stream.Send(&sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Chunk{Chunk: chunk}}))
	}}
	sink := newSink(chunks, false)
	truncated, err := readRange(call, stream, limit, sink)
	if err == nil {
		err = chunks.flush()
	}
	return call.finish(err, sink, truncated)
}

// readRange sends the header, then the first limit bytes of the file through
// sink; limit zero reads to the end.
func readRange(call *fileCall, stream grpc.ServerStreamingServer[sandboxv1.ReadFileResponse], limit int64, sink io.Writer) (bool, error) {
	reader, info, err := call.session.sandbox.OpenFile(call.ctx, call.path)
	if err != nil {
		return false, err
	}
	defer reader.Close()
	if !info.Mode().IsRegular() {
		return false, errNotRegular
	}
	truncated := limit > 0 && limit < info.Size()
	header := &sandboxv1.ReadFileHeader{Size: info.Size(), Truncated: truncated}
	if err := call.streamErr(stream.Send(&sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Header{Header: header}})); err != nil {
		return false, err
	}
	var source io.Reader = reader
	if limit > 0 {
		source = io.LimitReader(reader, limit)
	}
	_, err = io.Copy(sink, source)
	return truncated, err
}

func (svc *service) ReadLines(request *sandboxv1.ReadLinesRequest, stream grpc.ServerStreamingServer[sandboxv1.ReadLinesResponse]) error {
	invalid := ""
	if request.GetFirstLine() < 1 || request.GetMaxLines() < 1 || request.GetMaxLineBytes() < 0 {
		invalid = "line window out of range"
	}
	call, err := svc.beginFile(stream.Context(), kindFileRead, request.GetPath(), invalid, fileWindow{
		FirstLine: request.GetFirstLine(), MaxLines: request.GetMaxLines(), MaxLineBytes: request.GetMaxLineBytes(),
	})
	if err != nil {
		return err
	}
	reader, info, err := svc.session.sandbox.OpenFile(call.ctx, call.path)
	if err == nil && !info.Mode().IsRegular() {
		reader.Close()
		err = errNotRegular
	}
	if err != nil {
		return call.finish(err, nil, false)
	}
	defer reader.Close()
	chunks := &chunkWriter{send: func(chunk []byte) error {
		return call.streamErr(stream.Send(&sandboxv1.ReadLinesResponse{Part: &sandboxv1.ReadLinesResponse_Chunk{Chunk: chunk}}))
	}}
	sink := newSink(chunks, false)
	summary, err := readLines(reader, sink, request.GetFirstLine(), request.GetMaxLines(), request.GetMaxLineBytes())
	if summary != nil {
		total := summary.GetTotalLines()
		call.window.TotalLines = &total
	}
	if err == nil {
		err = chunks.flush()
	}
	if err == nil {
		summary.Size = info.Size()
		err = call.streamErr(stream.Send(&sandboxv1.ReadLinesResponse{Part: &sandboxv1.ReadLinesResponse_Summary{Summary: summary}}))
	}
	return call.finish(err, sink, summary.GetMore())
}

// readLines is `sed -n 'first,lastp' | cut -b1-clamp` plus `wc -l` plus a
// trailing-newline check, in one pass. It writes the window as it goes and
// holds no more than one bufio buffer: a line longer than that arrives in
// pieces and only the clamped prefix is kept.
func readLines(reader io.Reader, window io.Writer, first, count, clamp int64) (*sandboxv1.ReadLinesSummary, error) {
	buffered := bufio.NewReader(reader)
	var total, written int64 // newlines seen; bytes kept of the current line
	var last byte
	for {
		chunk, err := buffered.ReadSlice('\n')
		if len(chunk) > 0 {
			last = chunk[len(chunk)-1]
			line := total + 1
			// Differences, not sums, so no bound near MaxInt64 overflows.
			if line >= first && line-first < count {
				body, newline := bytes.CutSuffix(chunk, []byte{'\n'})
				if clamp > 0 && int64(len(body)) > clamp-written {
					body = body[:max(clamp-written, 0)]
				}
				if _, writeErr := window.Write(body); writeErr != nil {
					return nil, writeErr
				}
				written += int64(len(body))
				if newline {
					if _, writeErr := window.Write(newlineByte); writeErr != nil {
						return nil, writeErr
					}
				}
			}
			if last == '\n' {
				total++
				written = 0
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return &sandboxv1.ReadLinesSummary{TotalLines: total, EndsWithNewline: last == '\n', More: total-first >= count}, nil
}

func (svc *service) WriteFile(stream grpc.ClientStreamingServer[sandboxv1.WriteFileRequest, sandboxv1.WriteFileResponse]) error {
	first, err := stream.Recv()
	header := first.GetHeader()
	invalid := ""
	if err != nil || header == nil || header.GetSize() < 0 {
		invalid = errWriteStream.Error()
	}
	call, err := svc.beginFile(stream.Context(), kindFileWrite, header.GetPath(), invalid, fileWindow{})
	if err != nil {
		return err
	}
	sink := newSink(nil, true)
	recv := func() (*sandboxv1.WriteFileRequest, error) {
		message, err := stream.Recv()
		return message, call.streamErr(err)
	}
	content := &streamReader{recv: recv, remaining: header.GetSize(), sink: sink}
	created, err := svc.session.sandbox.WriteFile(call.ctx, call.path, content, header.GetSize())
	if err == nil {
		// The file changed, so a response the client is no longer there to
		// receive does not change the record.
		_ = stream.SendAndClose(&sandboxv1.WriteFileResponse{BytesWritten: header.GetSize(), Created: created})
	}
	return call.finish(err, sink, false)
}

// streamReader is the body of a WriteFile stream as the target reads it. It
// enforces the header's size: a byte past it, a second header, or the client
// closing early is errWriteStream, so the target never commits a body the
// client did not send exactly. io.EOF comes only from the client's half-close.
type streamReader struct {
	recv      func() (*sandboxv1.WriteFileRequest, error)
	remaining int64
	pending   []byte
	sink      io.Writer
}

func (reader *streamReader) Read(buffer []byte) (int, error) {
	for len(reader.pending) == 0 {
		message, err := reader.recv()
		if errors.Is(err, io.EOF) {
			if reader.remaining != 0 {
				return 0, errWriteStream
			}
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		chunk := message.GetChunk()
		if message.GetHeader() != nil || int64(len(chunk)) > reader.remaining {
			return 0, errWriteStream
		}
		reader.remaining -= int64(len(chunk))
		reader.pending = chunk
		_, _ = reader.sink.Write(chunk)
	}
	read := copy(buffer, reader.pending)
	reader.pending = reader.pending[read:]
	return read, nil
}

// recordFile writes one audit record for a file procedure. A file call has
// no exit code, so the field carries refusedExitCode; status says what
// happened. Written content follows the stdin rules; a read records only its
// size and, when completed, the digest of what was sent.
func (call *fileCall) recordFile(sink *contentSink, truncated bool, duration int64, statusText, message string) {
	session, kind := call.session, call.kind
	record := toolCallRecord{ToolCallRecord: bridgekit.ToolCallRecord{
		OperationClass: kind, Path: call.path,
		ExitCode: refusedExitCode, StartedAt: bridgekit.FormatTime(call.started), DurationMS: duration, Status: statusText, Error: message,
		StdinEncoding: "utf-8", ToolCallID: call.toolCallID,
	}, Truncated: truncated, fileWindow: call.window}
	raw := rawGRPCRecord{OperationClass: kind, Path: call.path}
	if sink != nil {
		if statusText == "completed" {
			record.SHA256 = hex.EncodeToString(sink.hash.Sum(nil))
			raw.SHA256 = record.SHA256
		}
		if kind == kindFileWrite {
			record.StdinBytes, raw.StdinBytes = sink.count, sink.count
			record.Stdin, record.StdinEncoding = session.describeInput(sink.kept.Bytes(), sink.count)
			raw.Stdin = sink.kept.Bytes()
			if !sink.keep {
				raw.Stdin = []byte(sizeOmitted(sink.count))
			}
		} else {
			record.StdoutBytes, raw.ReadBytes = sink.count, sink.count
		}
	}
	session.writeRecord(record, raw)
}
