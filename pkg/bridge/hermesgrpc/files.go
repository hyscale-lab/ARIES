package hermesgrpc

// The four file procedures. The bridge owns policy here — authorization, the
// absolute-path rule, status mapping and the audit record — and the sandbox
// owns how bytes land, through the narrow fileSandbox capability. A sandbox
// without that capability gets every file call recorded and answered
// UNIMPLEMENTED, so the whole chain can be observed before any sandbox
// implements it. File content streams in chunks and has no size bound: the
// bridge holds one chunk at a time, unless the profile retains content.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"io/fs"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fileSandbox is the file capability a sandbox may offer this bridge. The
// postconditions each method must meet are documented on the procedures in
// sandbox.proto; errors use the fs sentinels (fs.ErrNotExist, fs.ErrPermission)
// and syscall.EISDIR/ENOTDIR so no sandbox needs to import this package.
type fileSandbox interface {
	StatFile(ctx context.Context, path string) (fs.FileInfo, error)
	// OpenFile streams the whole file; the bridge reads only what it needs
	// and closes the reader early.
	OpenFile(ctx context.Context, path string) (io.ReadCloser, fs.FileInfo, error)
	// WriteFile lands the write only if content yields exactly size bytes and
	// then io.EOF; the bridge's reader waits for the client to finish there.
	WriteFile(ctx context.Context, path string, content io.Reader, size int64) (created bool, err error)
}

const (
	kindFileStat  = "file_stat"
	kindFileRead  = "file_read"
	kindFileLines = "file_lines"
	kindFileWrite = "file_write"

	// chunkBytes is the content carried by one stream message.
	chunkBytes = 64 << 10
)

// fileCall is one authorized file procedure: the sandbox capability and a
// context that both revocation and the client going away cancel.
type fileCall struct {
	session *bridgeSession
	files   fileSandbox
	kind    string
	path    string
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
}

func (svc *service) beginFile(ctx context.Context, kind, filePath string) (*fileCall, error) {
	session := svc.session
	if session.isRevoked() {
		session.recordFile(kind, filePath, nil, false, 0, "rejected", "session revoked")
		return nil, status.Error(codes.Unavailable, "session revoked")
	}
	if !path.IsAbs(filePath) || strings.ContainsRune(filePath, 0) {
		session.recordFile(kind, filePath, nil, false, 0, "rejected", "path must be absolute")
		return nil, status.Error(codes.InvalidArgument, "path must be absolute")
	}
	files, ok := session.sandbox.(fileSandbox)
	if !ok {
		session.recordFile(kind, filePath, nil, false, 0, "unimplemented", "sandbox offers no file access")
		session.logger.WithFields(logrus.Fields{"kind": kind, "path": filePath}).Info("Hermes gRPC file call; sandbox offers no file access")
		return nil, status.Error(codes.Unimplemented, "sandbox offers no file access")
	}
	callCtx, cancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-svc.serveCtx.Done():
			cancel()
		case <-callCtx.Done():
		}
	}()
	return &fileCall{session: session, files: files, kind: kind, path: filePath, ctx: callCtx, cancel: cancel, started: time.Now()}, nil
}

// newSink returns the contentSink for this call's content, passing it on to
// next (the stream, for reads) and keeping it when the profile asked.
func (call *fileCall) newSink(next io.Writer) *contentSink {
	sink := &contentSink{next: next, hash: sha256.New()}
	if call.session.retainContent {
		sink.kept = &bytes.Buffer{}
	}
	return sink
}

// finish records the outcome and converts a sandbox error to a status.
func (call *fileCall) finish(err error, sink *contentSink, truncated bool) error {
	defer call.cancel()
	err = bridgekit.WithCancellation(call.ctx, err)
	statusText, message := "completed", ""
	var result error
	if err != nil {
		call.session.RecordRevocationError(err)
		result = fileStatus(err)
		statusText, message = strings.ToLower(status.Code(result).String()), status.Convert(result).Message()
	}
	duration := time.Since(call.started).Milliseconds()
	call.session.recordFile(call.kind, call.path, sink, truncated, duration, statusText, message)
	var moved int64
	if sink != nil {
		moved = sink.count
	}
	call.session.logger.WithFields(logrus.Fields{
		"kind": call.kind, "status": statusText, "bytes": moved, "truncated": truncated, "duration_ms": duration,
	}).Info("Hermes gRPC file call")
	return result
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
// hashes them for the audit, keeps them when the profile asked, and passes
// them on to next when there is one.
type contentSink struct {
	next  io.Writer
	hash  hash.Hash
	kept  *bytes.Buffer
	count int64
}

func (sink *contentSink) Write(content []byte) (int, error) {
	if sink.next != nil {
		if written, err := sink.next.Write(content); err != nil {
			return written, err
		}
	}
	sink.hash.Write(content)
	if sink.kept != nil {
		sink.kept.Write(content)
	}
	sink.count += int64(len(content))
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
	call, err := svc.beginFile(ctx, kindFileStat, request.GetPath())
	if err != nil {
		return nil, err
	}
	info, err := call.files.StatFile(call.ctx, call.path)
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
	offset, limit := request.GetOffset(), request.GetMaxBytes()
	if offset < 0 || limit < 0 {
		svc.session.recordFile(kindFileRead, request.GetPath(), nil, false, 0, "rejected", "offset or max_bytes out of range")
		return status.Error(codes.InvalidArgument, "offset or max_bytes out of range")
	}
	call, err := svc.beginFile(stream.Context(), kindFileRead, request.GetPath())
	if err != nil {
		return err
	}
	chunks := &chunkWriter{send: func(chunk []byte) error {
		return stream.Send(&sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Chunk{Chunk: chunk}})
	}}
	sink := call.newSink(chunks)
	truncated, err := readRange(call, stream, offset, limit, sink)
	if err == nil {
		err = chunks.flush()
	}
	return call.finish(err, sink, truncated)
}

// readRange sends the header, then bytes [offset, offset+limit) of the file
// through sink; limit zero reads to the end.
func readRange(call *fileCall, stream grpc.ServerStreamingServer[sandboxv1.ReadFileResponse], offset, limit int64, sink io.Writer) (bool, error) {
	reader, info, err := call.files.OpenFile(call.ctx, call.path)
	if err != nil {
		return false, err
	}
	defer reader.Close()
	if !info.Mode().IsRegular() {
		return false, errNotRegular
	}
	truncated := limit > 0 && offset+limit < info.Size()
	header := &sandboxv1.ReadFileHeader{Size: info.Size(), Truncated: truncated}
	if err := stream.Send(&sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Header{Header: header}}); err != nil {
		return false, err
	}
	if _, err := io.CopyN(io.Discard, reader, offset); err != nil && !errors.Is(err, io.EOF) {
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
	if request.GetFirstLine() < 1 || request.GetMaxLines() < 1 || request.GetMaxLineBytes() < 0 {
		svc.session.recordFile(kindFileLines, request.GetPath(), nil, false, 0, "rejected", "line window out of range")
		return status.Error(codes.InvalidArgument, "line window out of range")
	}
	call, err := svc.beginFile(stream.Context(), kindFileLines, request.GetPath())
	if err != nil {
		return err
	}
	reader, info, err := call.files.OpenFile(call.ctx, call.path)
	if err == nil && !info.Mode().IsRegular() {
		reader.Close()
		err = errNotRegular
	}
	if err != nil {
		return call.finish(err, nil, false)
	}
	defer reader.Close()
	chunks := &chunkWriter{send: func(chunk []byte) error {
		return stream.Send(&sandboxv1.ReadLinesResponse{Part: &sandboxv1.ReadLinesResponse_Chunk{Chunk: chunk}})
	}}
	sink := call.newSink(chunks)
	summary, err := readLines(reader, sink, request.GetFirstLine(), request.GetMaxLines(), request.GetMaxLineBytes())
	if err == nil {
		err = chunks.flush()
	}
	if err == nil {
		summary.Size = info.Size()
		err = stream.Send(&sandboxv1.ReadLinesResponse{Part: &sandboxv1.ReadLinesResponse_Summary{Summary: summary}})
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
			if line >= first && line < first+count {
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
	return &sandboxv1.ReadLinesSummary{TotalLines: total, EndsWithNewline: last == '\n', More: total > first+count-1}, nil
}

func (svc *service) WriteFile(stream grpc.ClientStreamingServer[sandboxv1.WriteFileRequest, sandboxv1.WriteFileResponse]) error {
	first, err := stream.Recv()
	header := first.GetHeader()
	if err != nil || header == nil || header.GetSize() < 0 {
		svc.session.recordFile(kindFileWrite, header.GetPath(), nil, false, 0, "rejected", errWriteStream.Error())
		return status.Error(codes.InvalidArgument, errWriteStream.Error())
	}
	call, err := svc.beginFile(stream.Context(), kindFileWrite, header.GetPath())
	if err != nil {
		return err
	}
	sink := call.newSink(nil)
	content := &streamReader{recv: stream.Recv, remaining: header.GetSize(), sink: sink}
	created, err := call.files.WriteFile(call.ctx, call.path, content, header.GetSize())
	if err == nil {
		err = stream.SendAndClose(&sandboxv1.WriteFileResponse{BytesWritten: header.GetSize(), Created: created})
	}
	return call.finish(err, sink, false)
}

// streamReader is the body of a WriteFile stream as the sandbox reads it. It
// enforces the header's size: a byte past it, a second header, or the client
// closing early is errWriteStream, so the sandbox never commits a body the
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

// recordFile writes one audit record for a file procedure. File procedures
// have no exit code, so the field carries -1 as it does for refused calls;
// status says what happened. A completed call records the content's size and
// sha256, and the content itself, base64, when the profile asked for it.
func (session *bridgeSession) recordFile(kind, filePath string, sink *contentSink, truncated bool, duration int64, statusText, message string) {
	record := toolCallRecord{
		OperationClass: kind, Path: filePath, Truncated: truncated,
		ExitCode: -1, DurationMS: duration, Status: statusText, Error: message,
		StdinEncoding: "utf-8",
	}
	if sink != nil {
		if kind == kindFileWrite {
			record.StdinBytes = sink.count
		} else {
			record.StdoutBytes = sink.count
		}
		if statusText == "completed" {
			record.SHA256 = hex.EncodeToString(sink.hash.Sum(nil))
			if sink.kept != nil && sink.kept.Len() != 0 {
				record.ContentRaw = base64.StdEncoding.EncodeToString(sink.kept.Bytes())
			}
		}
	}
	session.writeRecord(record)
}
