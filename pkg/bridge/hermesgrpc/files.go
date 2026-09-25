package hermesgrpc

// The four file procedures. The bridge owns policy here — authorization, the
// absolute-path rule, bounds, status mapping and the audit record — and the
// sandbox owns how bytes land, through the narrow fileSandbox capability. A
// sandbox without that capability gets every file call recorded and answered
// UNIMPLEMENTED, so the whole chain can be observed before any sandbox
// implements it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"github.com/sirupsen/logrus"
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
	WriteFile(ctx context.Context, path string, content io.Reader, size int64) (created bool, err error)
}

const (
	kindFileStat  = "file_stat"
	kindFileRead  = "file_read"
	kindFileLines = "file_lines"
	kindFileWrite = "file_write"
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
		session.recordFile(kind, filePath, 0, 0, false, 0, "rejected", "session revoked", nil)
		return nil, status.Error(codes.Unavailable, "session revoked")
	}
	if !path.IsAbs(filePath) || strings.ContainsRune(filePath, 0) {
		session.recordFile(kind, filePath, 0, 0, false, 0, "rejected", "path must be absolute", nil)
		return nil, status.Error(codes.InvalidArgument, "path must be absolute")
	}
	files, ok := session.sandbox.(fileSandbox)
	if !ok {
		session.recordFile(kind, filePath, 0, 0, false, 0, "unimplemented", "sandbox offers no file access", nil)
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

// finish records the outcome and converts a sandbox error to a status. content
// is retained only when the profile asked for file content in the audit.
func (call *fileCall) finish(err error, inBytes, outBytes int64, truncated bool, content []byte) error {
	defer call.cancel()
	err = withCancellation(call.ctx, err)
	statusText, message := "completed", ""
	var result error
	if err != nil {
		call.session.recordRevocationError(err)
		result = fileStatus(err)
		statusText, message = strings.ToLower(status.Code(result).String()), status.Convert(result).Message()
		content = nil
	}
	duration := time.Since(call.started).Milliseconds()
	call.session.recordFile(call.kind, call.path, inBytes, outBytes, truncated, duration, statusText, message, content)
	call.session.logger.WithFields(logrus.Fields{
		"kind": call.kind, "status": statusText, "in_bytes": inBytes, "out_bytes": outBytes,
		"truncated": truncated, "duration_ms": duration,
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
	case errors.Is(err, errWindowTooLarge):
		return status.Error(codes.ResourceExhausted, errWindowTooLarge.Error())
	case hasCancellationCause(err):
		return status.Error(codes.Canceled, "session canceled")
	default:
		return status.Error(codes.Internal, "sandbox file access failed")
	}
}

var (
	errNotRegular     = errors.New("not a regular file")
	errWindowTooLarge = errors.New("line window exceeds the bridge's bound")
)

func (svc *service) Stat(ctx context.Context, request *sandboxv1.StatRequest) (*sandboxv1.StatResponse, error) {
	call, err := svc.beginFile(ctx, kindFileStat, request.GetPath())
	if err != nil {
		return nil, err
	}
	info, err := call.files.StatFile(call.ctx, call.path)
	if errors.Is(err, fs.ErrNotExist) {
		return &sandboxv1.StatResponse{}, call.finish(nil, 0, 0, false, nil)
	}
	if err != nil {
		return nil, call.finish(err, 0, 0, false, nil)
	}
	response := &sandboxv1.StatResponse{Exists: true, Mode: uint32(info.Mode().Perm()), Type: sandboxv1.FileType_FILE_TYPE_OTHER}
	switch {
	case info.Mode().IsRegular():
		response.Type, response.Size = sandboxv1.FileType_FILE_TYPE_REGULAR, info.Size()
	case info.IsDir():
		response.Type = sandboxv1.FileType_FILE_TYPE_DIRECTORY
	}
	return response, call.finish(nil, 0, 0, false, nil)
}

func (svc *service) ReadFile(ctx context.Context, request *sandboxv1.ReadFileRequest) (*sandboxv1.ReadFileResponse, error) {
	limit := request.GetMaxBytes()
	if limit == 0 {
		limit = svc.session.outputLimit
	}
	if request.GetOffset() < 0 || limit < 0 || limit > svc.session.outputLimit {
		svc.session.recordFile(kindFileRead, request.GetPath(), 0, 0, false, 0, "rejected", "offset or max_bytes out of range", nil)
		return nil, status.Error(codes.InvalidArgument, "offset or max_bytes out of range")
	}
	call, err := svc.beginFile(ctx, kindFileRead, request.GetPath())
	if err != nil {
		return nil, err
	}
	content, size, err := readRange(call, request.GetOffset(), limit)
	if err != nil {
		return nil, call.finish(err, 0, 0, false, nil)
	}
	truncated := request.GetOffset()+int64(len(content)) < size
	return &sandboxv1.ReadFileResponse{Content: content, Size: size, Truncated: truncated},
		call.finish(nil, 0, int64(len(content)), truncated, content)
}

func readRange(call *fileCall, offset, limit int64) ([]byte, int64, error) {
	reader, info, err := call.files.OpenFile(call.ctx, call.path)
	if err != nil {
		return nil, 0, err
	}
	defer reader.Close()
	if !info.Mode().IsRegular() {
		return nil, 0, errNotRegular
	}
	if _, err := io.CopyN(io.Discard, reader, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, err
	}
	content, err := io.ReadAll(io.LimitReader(reader, limit))
	return content, info.Size(), err
}

func (svc *service) ReadLines(ctx context.Context, request *sandboxv1.ReadLinesRequest) (*sandboxv1.ReadLinesResponse, error) {
	if request.GetFirstLine() < 1 || request.GetMaxLines() < 1 || request.GetMaxLineBytes() < 0 {
		svc.session.recordFile(kindFileLines, request.GetPath(), 0, 0, false, 0, "rejected", "line window out of range", nil)
		return nil, status.Error(codes.InvalidArgument, "line window out of range")
	}
	call, err := svc.beginFile(ctx, kindFileLines, request.GetPath())
	if err != nil {
		return nil, err
	}
	reader, info, err := call.files.OpenFile(call.ctx, call.path)
	if err == nil && !info.Mode().IsRegular() {
		reader.Close()
		err = errNotRegular
	}
	if err != nil {
		return nil, call.finish(err, 0, 0, false, nil)
	}
	defer reader.Close()
	window, err := readLines(reader, request.GetFirstLine(), request.GetMaxLines(), request.GetMaxLineBytes(), svc.session.outputLimit)
	if err != nil {
		return nil, call.finish(err, 0, 0, false, nil)
	}
	window.Size = info.Size()
	return window, call.finish(nil, 0, int64(len(window.Content)), window.More, window.Content)
}

// readLines is `sed -n 'first,lastp' | cut -b1-clamp` plus `wc -l` plus a
// trailing-newline check, in one pass. It never holds more than the window: a
// line longer than the bufio buffer arrives in pieces and only the clamped
// prefix is kept.
func readLines(reader io.Reader, first, count, clamp, limit int64) (*sandboxv1.ReadLinesResponse, error) {
	buffered := bufio.NewReader(reader)
	var window bytes.Buffer
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
				window.Write(body)
				written += int64(len(body))
				if newline {
					window.WriteByte('\n')
				}
				if int64(window.Len()) > limit {
					return nil, errWindowTooLarge
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
	return &sandboxv1.ReadLinesResponse{
		Content: window.Bytes(), TotalLines: total, EndsWithNewline: last == '\n',
		More: total > first+count-1,
	}, nil
}

func (svc *service) WriteFile(ctx context.Context, request *sandboxv1.WriteFileRequest) (*sandboxv1.WriteFileResponse, error) {
	content := request.GetContent()
	if int64(len(content)) > svc.session.outputLimit {
		svc.session.recordFile(kindFileWrite, request.GetPath(), int64(len(content)), 0, false, 0, "rejected", "content exceeds the bridge's bound", nil)
		return nil, status.Error(codes.ResourceExhausted, "content exceeds the bridge's bound")
	}
	call, err := svc.beginFile(ctx, kindFileWrite, request.GetPath())
	if err != nil {
		return nil, err
	}
	created, err := call.files.WriteFile(call.ctx, call.path, bytes.NewReader(content), int64(len(content)))
	if err != nil {
		return nil, call.finish(err, int64(len(content)), 0, false, nil)
	}
	return &sandboxv1.WriteFileResponse{BytesWritten: int64(len(content)), Created: created},
		call.finish(nil, int64(len(content)), 0, false, content)
}

// recordFile writes one audit record for a file procedure. File procedures
// have no exit code, so the field carries -1 as it does for refused calls;
// status says what happened. content is kept only when the profile asked for
// it, base64 because it may be binary.
func (session *bridgeSession) recordFile(kind, filePath string, inBytes, outBytes int64, truncated bool, duration int64, statusText, message string, content []byte) {
	record := toolCallRecord{
		OperationClass: kind, Path: filePath,
		StdinBytes: inBytes, StdoutBytes: outBytes, Truncated: truncated,
		ExitCode: -1, DurationMS: duration, Status: statusText, Error: message,
		StdinEncoding: "utf-8",
	}
	if session.retainContent && len(content) != 0 && int64(len(content)) <= maxRecordedInputBytes {
		record.ContentRaw = base64.StdEncoding.EncodeToString(content)
	}
	session.writeRecord(record)
}
