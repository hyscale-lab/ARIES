package grpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const replayTestDeadline = time.Second * 5

// The fake is a local gRPC endpoint. It exercises the generated client, not a
// mock client that could conceal stream framing or metadata errors.
type replayServer struct {
	sandboxv1.UnimplementedSandboxServer
	exec      func(context.Context, *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error)
	stat      func(context.Context, *sandboxv1.StatRequest) (*sandboxv1.StatResponse, error)
	readFile  func(*sandboxv1.ReadFileRequest, grpc.ServerStreamingServer[sandboxv1.ReadFileResponse]) error
	readLines func(*sandboxv1.ReadLinesRequest, grpc.ServerStreamingServer[sandboxv1.ReadLinesResponse]) error
	writeFile func(grpc.ClientStreamingServer[sandboxv1.WriteFileRequest, sandboxv1.WriteFileResponse]) error
}

func (server *replayServer) Exec(ctx context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
	return server.exec(ctx, request)
}
func (server *replayServer) Stat(ctx context.Context, request *sandboxv1.StatRequest) (*sandboxv1.StatResponse, error) {
	return server.stat(ctx, request)
}
func (server *replayServer) ReadFile(request *sandboxv1.ReadFileRequest, stream grpc.ServerStreamingServer[sandboxv1.ReadFileResponse]) error {
	return server.readFile(request, stream)
}
func (server *replayServer) ReadLines(request *sandboxv1.ReadLinesRequest, stream grpc.ServerStreamingServer[sandboxv1.ReadLinesResponse]) error {
	return server.readLines(request, stream)
}
func (server *replayServer) WriteFile(stream grpc.ClientStreamingServer[sandboxv1.WriteFileRequest, sandboxv1.WriteFileResponse]) error {
	return server.writeFile(stream)
}

func replayEndpoint(t *testing.T, server *replayServer) sandboxv1.SandboxClient {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(endpoint, server)
	go func() { _ = endpoint.Serve(listener) }()
	t.Cleanup(endpoint.Stop)
	return dial(t, core.ToolEndpoint{Address: listener.Addr().String()})
}

func expectedReplayCall(operation string, messages ...proto.Message) replay.Call {
	method := map[string]string{
		"Exec": sandboxv1.Sandbox_Exec_FullMethodName, "Stat": sandboxv1.Sandbox_Stat_FullMethodName,
		"ReadFile": sandboxv1.Sandbox_ReadFile_FullMethodName, "ReadLines": sandboxv1.Sandbox_ReadLines_FullMethodName,
		"WriteFile": sandboxv1.Sandbox_WriteFile_FullMethodName,
	}[operation]
	outcome := newResponseOutcome(method)
	for _, message := range messages {
		outcome.message(message)
	}
	outcome.field([]byte(codes.OK.String()))
	outcome.field(nil)
	call := replay.Call{Operation: operation, ResponseSHA256: hex.EncodeToString(outcome.hash.Sum(nil)), ResponseBytes: outcome.bytes, ResponseCode: codes.OK.String()}
	if operation == "Exec" && len(messages) != 0 {
		call.ExitCode = messages[0].(*sandboxv1.ExecResponse).GetExitCode()
	}
	return call
}

func TestReplayCallCarriesSourceSequenceSeparateFromToolCallID(t *testing.T) {
	const firstSourceSequence uint64 = 1
	const decimalBase = 10
	const oneMetadataValue = 1
	const sourceSequenceKey = "aries-replay-source-sequence"
	response := &sandboxv1.ExecResponse{}
	call := expectedReplayCall("Exec", response)
	call.Sequence, call.ToolCallID = firstSourceSequence, testToolCallID
	server := &replayServer{exec: func(ctx context.Context, _ *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
		sequence := metadata.ValueFromIncomingContext(ctx, sourceSequenceKey)
		toolID := metadata.ValueFromIncomingContext(ctx, toolCallIDKey)
		if len(sequence) != oneMetadataValue || sequence[0] != strconv.FormatUint(firstSourceSequence, decimalBase) || len(toolID) != oneMetadataValue || toolID[0] != call.ToolCallID {
			return nil, status.Error(codes.InvalidArgument, "replay metadata changed")
		}
		return response, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	if err := ReplayCalls(ctx, replayTestDeadline, replayEndpoint(t, server), replay.Plan{Calls: []replay.Call{call}}); err != nil {
		t.Fatal(err)
	}
}

func TestReplayCallsWriteFileFramesAndChecksByteCount(t *testing.T) {
	body := bytes.Repeat([]byte("write payload"), chunkBytes/len("write payload")+1)
	response := &sandboxv1.WriteFileResponse{BytesWritten: int64(len(body)), Created: true}
	frames := make(chan []int, 1)
	var wrongCount atomic.Bool
	call := expectedReplayCall("WriteFile", response)
	call.Path, call.Body, call.ToolCallID = "/app/output", body, "write-origin"
	server := &replayServer{writeFile: func(stream grpc.ClientStreamingServer[sandboxv1.WriteFileRequest, sandboxv1.WriteFileResponse]) error {
		if values := metadata.ValueFromIncomingContext(stream.Context(), toolCallIDKey); len(values) != 1 || values[0] != call.ToolCallID {
			return status.Error(codes.InvalidArgument, "tool call ID changed")
		}
		first, err := stream.Recv()
		if err != nil || first.GetHeader().GetPath() != call.Path || first.GetHeader().GetSize() != int64(len(body)) {
			return status.Error(codes.InvalidArgument, "wrong write header")
		}
		var received bytes.Buffer
		var sizes []int
		for {
			part, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil || part.GetHeader() != nil {
				return status.Error(codes.InvalidArgument, "wrong write frame")
			}
			sizes = append(sizes, len(part.GetChunk()))
			_, _ = received.Write(part.GetChunk())
		}
		if !bytes.Equal(received.Bytes(), body) {
			return status.Error(codes.InvalidArgument, "write bytes changed")
		}
		written := &sandboxv1.WriteFileResponse{BytesWritten: response.BytesWritten, Created: response.Created}
		if wrongCount.Load() {
			written.BytesWritten--
		} else {
			frames <- sizes
		}
		return stream.SendAndClose(written)
	}}
	client := replayEndpoint(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	if err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}}); err != nil {
		t.Fatal(err)
	}
	sizes := <-frames
	if len(sizes) != 2 || sizes[0] != chunkBytes || sizes[1] != len(body)-chunkBytes {
		t.Fatalf("write frames = %v", sizes)
	}
	wrongCount.Store(true)
	if err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}}); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("wrong byte count accepted with debug on: %v", err)
	}
}

func TestReplayCallsNormalizeReadFramesAndCompareFullOutcome(t *testing.T) {
	body := bytes.Repeat([]byte("body"), chunkBytes/len("body")+1)
	header := &sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Header{Header: &sandboxv1.ReadFileHeader{Size: int64(len(body))}}}
	chunk := func(content []byte) *sandboxv1.ReadFileResponse {
		return &sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Chunk{Chunk: content}}
	}
	call := expectedReplayCall("ReadFile", header, chunk(body[:chunkBytes]), chunk(body[chunkBytes:]))
	call.Path = "/app/file"
	call.MaxBytes = int64(len(body))
	frame := len(body) / 3
	server := &replayServer{readFile: func(request *sandboxv1.ReadFileRequest, stream grpc.ServerStreamingServer[sandboxv1.ReadFileResponse]) error {
		if request.GetPath() != call.Path || request.GetMaxBytes() != call.MaxBytes {
			return status.Error(codes.InvalidArgument, "read request changed")
		}
		if err := stream.Send(header); err != nil {
			return err
		}
		for start := 0; start < len(body); start += frame {
			if err := stream.Send(chunk(body[start:min(start+frame, len(body))])); err != nil {
				return err
			}
		}
		return nil
	}}
	client := replayEndpoint(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	if err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}}); err != nil {
		t.Fatalf("same body with different frames: %v", err)
	}
	header.GetHeader().Truncated = true
	if err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}}); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("changed header accepted: %v", err)
	}
}

func TestReplayCallsAllowReadFileBodyBeforeHeaderAsHashDrift(t *testing.T) {
	body := []byte("file contents")
	header := &sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Header{Header: &sandboxv1.ReadFileHeader{Size: int64(len(body))}}}
	chunk := &sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Chunk{Chunk: body}}
	call := expectedReplayCall("ReadFile", header, chunk)
	call.Path = "/app/file"
	server := &replayServer{readFile: func(_ *sandboxv1.ReadFileRequest, stream grpc.ServerStreamingServer[sandboxv1.ReadFileResponse]) error {
		if err := stream.Send(chunk); err != nil {
			return err
		}
		return stream.Send(header)
	}}
	client := replayEndpoint(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	if err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}}); err != nil {
		t.Fatalf("hash-only drift stopped replay: %v", err)
	}
}

func TestReplayCallsStopOnMismatchAndCancelInFlight(t *testing.T) {
	const nextOffset = 100 * time.Millisecond
	for _, mismatchOffset := range []time.Duration{0, nextOffset} {
		t.Run(mismatchOffset.String(), func(t *testing.T) {
			entered := make(chan struct{})
			canceled := make(chan struct{})
			var mu sync.Mutex
			var started []string
			server := &replayServer{exec: func(ctx context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
				mu.Lock()
				started = append(started, request.GetScript())
				mu.Unlock()
				switch request.GetScript() {
				case "slow":
					close(entered)
					<-ctx.Done()
					close(canceled)
					return nil, status.Error(codes.Canceled, "slow call canceled")
				case "bad":
					select {
					case <-entered:
						return &sandboxv1.ExecResponse{Stdout: []byte("changed")}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				default:
					return &sandboxv1.ExecResponse{}, nil
				}
			}}
			call := expectedReplayCall("Exec", &sandboxv1.ExecResponse{})
			first, second, third := call, call, call
			first.Script, first.Sequence = "slow", 1
			second.Script, second.Sequence, second.StartOffset = "bad", 2, mismatchOffset
			third.Script, third.Sequence, third.StartOffset = "later", 3, 2*nextOffset
			client := replayEndpoint(t, server)
			ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
			defer cancel()
			plan := replay.Plan{Calls: []replay.Call{first, second, third}}
			if mismatchOffset != 0 {
				origin := time.Now().Add(-nextOffset)
				plan.ReplayAt = &origin
			}
			if err := ReplayCalls(ctx, replayTestDeadline, client, plan); err == nil || !strings.Contains(err.Error(), "mismatch") {
				t.Fatalf("changed response accepted: %v", err)
			}
			select {
			case <-canceled:
			case <-time.After(replayTestDeadline):
				t.Fatal("in-flight call did not receive cancellation")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(started) != 2 {
				t.Fatalf("calls started after mismatch: %v", started)
			}
		})
	}
}

func TestReplayCallsApplyIndividualTimeoutBeforeLaterCall(t *testing.T) {
	const callTimeout = 100 * time.Millisecond
	const laterOffset = callTimeout + callTimeout
	const deadlineMessage = "context deadline exceeded"
	const oneResult = 1
	firstCanceled := make(chan struct{})
	firstBudget := make(chan time.Duration, oneResult)
	laterStarted := make(chan struct{})
	server := &replayServer{exec: func(ctx context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
		if request.GetScript() == "first" {
			deadline, _ := ctx.Deadline()
			firstBudget <- time.Until(deadline)
			<-ctx.Done()
			close(firstCanceled)
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		select {
		case <-firstCanceled:
			close(laterStarted)
			return &sandboxv1.ExecResponse{}, nil
		default:
			return nil, status.Error(codes.FailedPrecondition, "first call exceeded its allowance")
		}
	}}
	outcome := newResponseOutcome(sandboxv1.Sandbox_Exec_FullMethodName)
	outcome.field([]byte(codes.DeadlineExceeded.String()))
	outcome.field([]byte(deadlineMessage))
	first := replay.Call{Operation: "Exec", Script: "first", ResponseCode: codes.DeadlineExceeded.String(), ResponseBytes: outcome.bytes}
	later := expectedReplayCall("Exec", &sandboxv1.ExecResponse{})
	later.Script, later.StartOffset = "later", laterOffset
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	plan := replay.Plan{Calls: []replay.Call{first, later}}
	if err := ReplayCalls(ctx, callTimeout, replayEndpoint(t, server), plan); err != nil {
		t.Fatalf("recorded timeout blocked later call: %v", err)
	}
	if budget := <-firstBudget; budget <= 0 || budget > callTimeout {
		t.Fatalf("first RPC allowance = %s, want at most %s", budget, callTimeout)
	}
	select {
	case <-laterStarted:
	default:
		t.Fatal("later call did not run after the expected timeout")
	}
}

func TestReplayCallsScheduleOverlapAndFinalWait(t *testing.T) {
	const step = 100 * time.Millisecond
	const finalWait = 2 * step
	firstStarted := make(chan time.Time, 1)
	firstFinished := make(chan time.Time, 1)
	secondStarted := make(chan time.Time, 1)
	thirdStarted := make(chan time.Time, 1)
	releaseFirst := make(chan struct{})
	server := &replayServer{exec: func(ctx context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
		start := time.Now()
		switch request.GetScript() {
		case "first":
			firstStarted <- start
			select {
			case <-releaseFirst:
				// The first call finishes after the last call launches.
				<-time.After(step)
				firstFinished <- time.Now()
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case "second":
			secondStarted <- start
		case "third":
			thirdStarted <- start
			close(releaseFirst)
		}
		return &sandboxv1.ExecResponse{}, nil
	}}
	client := replayEndpoint(t, server)
	call := expectedReplayCall("Exec", &sandboxv1.ExecResponse{})
	first, second, third := call, call, call
	first.Script, second.Script, third.Script = "first", "second", "third"
	first.Sequence, second.Sequence, third.Sequence = 1, 2, 3
	second.StartOffset, third.StartOffset = step, 2*step
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	plan := replay.Plan{Calls: []replay.Call{first, second, third}, FinalWait: finalWait}
	testStart := time.Now()
	if err := ReplayCalls(ctx, replayTestDeadline, client, plan); err != nil {
		t.Fatalf("overlapping calls: %v", err)
	}
	returned := time.Now()
	origin, next, last := <-firstStarted, <-secondStarted, <-thirdStarted
	finished := <-firstFinished
	const earliestThirdStart = step + step/2
	if next.Sub(origin) < step/2 || last.Sub(next) > 2*step || last.Sub(testStart) < earliestThirdStart || last.Sub(origin) > 3*step {
		t.Fatalf("absolute starts: first = %v, second = %v, third = %v", origin, next, last)
	}
	if returned.Sub(finished) < finalWait {
		t.Fatalf("final wait started before last call ended: delay = %v, want at least %v", returned.Sub(finished), finalWait)
	}
}

func TestReplayCallsCompareExecStatAndReadLinesFields(t *testing.T) {
	execRequest := &sandboxv1.ExecRequest{Script: "run this script", Stdin: []byte("input text"), Login: true}
	linesRequest := &sandboxv1.ReadLinesRequest{Path: "/app/file", FirstLine: 1, MaxLines: 2, MaxLineBytes: int64(len("line"))}
	execResponse := &sandboxv1.ExecResponse{ExitCode: 3, Reason: sandboxv1.Reason_REASON_COMPLETED, Stdout: []byte("out"), Stderr: []byte("err"), Truncated: true}
	statResponse := &sandboxv1.StatResponse{Exists: true, Type: sandboxv1.FileType_FILE_TYPE_REGULAR, Size: 7, Mode: 0o600}
	linesChunk := &sandboxv1.ReadLinesResponse{Part: &sandboxv1.ReadLinesResponse_Chunk{Chunk: []byte("line\n")}}
	linesSummary := &sandboxv1.ReadLinesResponse{Part: &sandboxv1.ReadLinesResponse_Summary{Summary: &sandboxv1.ReadLinesSummary{TotalLines: 2, Size: 7, EndsWithNewline: true, More: true}}}
	server := &replayServer{
		exec: func(_ context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
			if !proto.Equal(request, execRequest) {
				return nil, status.Error(codes.InvalidArgument, "Exec request changed")
			}
			return execResponse, nil
		},
		stat: func(context.Context, *sandboxv1.StatRequest) (*sandboxv1.StatResponse, error) {
			return statResponse, nil
		},
		readLines: func(request *sandboxv1.ReadLinesRequest, stream grpc.ServerStreamingServer[sandboxv1.ReadLinesResponse]) error {
			if !proto.Equal(request, linesRequest) {
				return status.Error(codes.InvalidArgument, "ReadLines window changed")
			}
			if err := stream.Send(linesChunk); err != nil {
				return err
			}
			return stream.Send(linesSummary)
		},
	}
	client := replayEndpoint(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	for _, scenario := range []struct {
		name   string
		call   replay.Call
		change func()
	}{
		{"Exec stdout", expectedReplayCall("Exec", execResponse), func() { execResponse.Stdout = []byte("different") }},
		{"Exec exit", expectedReplayCall("Exec", execResponse), func() { execResponse.ExitCode++ }},
		{"Exec reason", expectedReplayCall("Exec", execResponse), func() { execResponse.Reason = sandboxv1.Reason_REASON_TIMED_OUT }},
		{"Exec stderr", expectedReplayCall("Exec", execResponse), func() { execResponse.Stderr = []byte("different") }},
		{"Exec truncated", expectedReplayCall("Exec", execResponse), func() { execResponse.Truncated = false }},
		{"Stat size", expectedReplayCall("Stat", statResponse), func() { statResponse.Size++ }},
		{"Stat type", expectedReplayCall("Stat", statResponse), func() { statResponse.Type = sandboxv1.FileType_FILE_TYPE_DIRECTORY }},
		{"Stat mode", expectedReplayCall("Stat", statResponse), func() { statResponse.Mode++ }},
		{"Stat existence", expectedReplayCall("Stat", statResponse), func() { statResponse.Exists = false }},
		{"ReadLines summary", expectedReplayCall("ReadLines", linesChunk, linesSummary), func() { linesSummary.GetSummary().More = false }},
		{"ReadLines content", expectedReplayCall("ReadLines", linesChunk, linesSummary), func() { linesChunk.GetPart().(*sandboxv1.ReadLinesResponse_Chunk).Chunk = []byte("other\n") }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			call := scenario.call
			switch call.Operation {
			case "Exec":
				call = expectedReplayCall("Exec", execResponse)
				call.Script, call.Body, call.Login = execRequest.Script, execRequest.Stdin, execRequest.Login
			case "Stat":
				call = expectedReplayCall("Stat", statResponse)
			case "ReadLines":
				call = expectedReplayCall("ReadLines", linesChunk, linesSummary)
				call.Path, call.FirstLine, call.MaxLines, call.MaxLineBytes = linesRequest.Path, linesRequest.FirstLine, linesRequest.MaxLines, linesRequest.MaxLineBytes
			}
			if err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}}); err != nil {
				t.Fatal(err)
			}
			scenario.change()
			err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}})
			strict := scenario.name == "Exec exit" || scenario.name == "Exec stdout" || scenario.name == "Exec stderr" || scenario.name == "Exec truncated" || scenario.name == "Stat existence" || scenario.name == "ReadLines summary" || scenario.name == "ReadLines content"
			if strict && (err == nil || !strings.Contains(err.Error(), "mismatch")) {
				t.Fatalf("strict response change accepted: %v", err)
			}
			if !strict && err != nil {
				t.Fatalf("hash-only response drift stopped replay: %v", err)
			}
		})
	}
}

func TestReplayCallsCompareGRPCErrorCodeAndMessage(t *testing.T) {
	const expectedMessage = "source failure"
	outcome := newResponseOutcome(sandboxv1.Sandbox_Stat_FullMethodName)
	outcome.field([]byte(codes.NotFound.String()))
	outcome.field([]byte(expectedMessage))
	call := replay.Call{Operation: "Stat", Path: "/app/missing", ResponseSHA256: hex.EncodeToString(outcome.hash.Sum(nil)), ResponseBytes: outcome.bytes, ResponseCode: codes.NotFound.String()}
	var changedCode, changedMessage atomic.Bool
	server := &replayServer{stat: func(_ context.Context, request *sandboxv1.StatRequest) (*sandboxv1.StatResponse, error) {
		if request.GetPath() != call.Path {
			return nil, status.Error(codes.InvalidArgument, "wrong path")
		}
		code, message := codes.NotFound, expectedMessage
		if changedCode.Load() {
			code = codes.Internal
		}
		if changedMessage.Load() {
			message = "different failure"
		}
		return nil, status.Error(code, message)
	}}
	client := replayEndpoint(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	plan := replay.Plan{Calls: []replay.Call{call}}
	if err := ReplayCalls(ctx, replayTestDeadline, client, plan); err != nil {
		t.Fatalf("recorded failure changed: %v", err)
	}
	changedCode.Store(true)
	if err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}}); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("changed status code accepted with debug on: %v", err)
	}
	changedCode.Store(false)
	changedMessage.Store(true)
	if err := ReplayCalls(ctx, replayTestDeadline, client, plan); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("changed error message accepted: %v", err)
	}
}

func TestReplayCallsDefaultHashDriftContinuesConcurrentCalls(t *testing.T) {
	const sourceSequence uint64 = 1
	const nextSequence uint64 = sourceSequence + 1
	const laterSequence uint64 = nextSequence + 1
	const laterOffset = time.Second
	source := &sandboxv1.ExecResponse{Stdout: []byte("git hash aaa")}
	changed := &sandboxv1.ExecResponse{Stdout: []byte("git hash bbb")}
	second := &sandboxv1.ExecResponse{}
	firstCall := expectedReplayCall("Exec", source)
	firstCall.Sequence, firstCall.Script, firstCall.ExitCode = sourceSequence, "first", source.GetExitCode()
	secondCall := expectedReplayCall("Exec", second)
	secondCall.Sequence, secondCall.Script = nextSequence, "second"
	laterCall := expectedReplayCall("Exec", second)
	laterCall.Sequence, laterCall.Script, laterCall.StartOffset = laterSequence, "later", laterOffset
	started := make(chan struct{})
	finished := make(chan struct{})
	laterStarted := make(chan struct{})
	server := &replayServer{exec: func(ctx context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
		switch request.GetScript() {
		case "first":
			close(started)
			<-finished
			return changed, nil
		case "second":
			<-started
			close(finished)
		case "later":
			close(laterStarted)
		}
		return second, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	plan := replay.Plan{Calls: []replay.Call{firstCall, secondCall, laterCall}}
	if err := ReplayCalls(ctx, replayTestDeadline, replayEndpoint(t, server), plan); err != nil {
		t.Fatalf("default hash drift blocked later call: %v", err)
	}
	select {
	case <-laterStarted:
	default:
		t.Fatal("later call did not run after response hash drift")
	}
}

func TestReplayCallsDefaultHashDriftKeepsStrictChecks(t *testing.T) {
	const sourceExitCode int32 = 1
	const changedExitCode int32 = sourceExitCode + 1
	source := &sandboxv1.ExecResponse{ExitCode: sourceExitCode, Stdout: []byte("git hash aaa")}
	response := proto.Clone(source).(*sandboxv1.ExecResponse)
	call := expectedReplayCall("Exec", source)
	call.ExitCode = sourceExitCode
	server := &replayServer{exec: func(context.Context, *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) { return response, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	client := replayEndpoint(t, server)
	response.Stdout = []byte("git hash bbb")
	for _, scenario := range []struct {
		name   string
		change func()
	}{
		{"exit code", func() { response.ExitCode = changedExitCode }},
		{"response length", func() { response.Stdout = append(response.Stdout, '!') }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			response = proto.Clone(source).(*sandboxv1.ExecResponse)
			response.Stdout = []byte("git hash bbb")
			scenario.change()
			if err := ReplayCalls(ctx, replayTestDeadline, client, replay.Plan{Calls: []replay.Call{call}}); err == nil || !strings.Contains(err.Error(), "mismatch") {
				t.Fatalf("strict replay change accepted: %v", err)
			}
		})
	}
	response = proto.Clone(source).(*sandboxv1.ExecResponse)
	response.Stdout = []byte("git hash bbb")
}

type scheduledReplayClient struct {
	sandboxv1.SandboxClient
	exec func(context.Context) (*sandboxv1.ExecResponse, error)
}

func (client scheduledReplayClient) Exec(ctx context.Context, _ *sandboxv1.ExecRequest, _ ...grpc.CallOption) (*sandboxv1.ExecResponse, error) {
	return client.exec(ctx)
}

func TestReplayCallsShareOriginAcrossDelayedCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const step = time.Second
		const callTimeout = step
		const finalWait = step
		started := time.Now()
		origin := started.Add(step)
		data, err := json.Marshal(map[string]any{"replay_at": origin})
		if err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name          string
			ready, offset time.Duration
			want          time.Duration
		}{
			{name: "first equal target", offset: step, want: step + step},
			{name: "delayed equal target", ready: step, offset: step, want: step + step},
			{name: "later global target", ready: step, offset: step + step, want: step + step + step},
			{name: "overdue target", ready: step + step + step, offset: step, want: step + step + step},
		}
		done := make(chan struct{}, len(tests))
		for _, test := range tests {
			go func() {
				defer func() { done <- struct{}{} }()
				// JSON crosses the same process boundary as a private staged plan.
				var plan replay.Plan
				if err := json.Unmarshal(data, &plan); err != nil {
					t.Error(err)
					return
				}
				call := expectedReplayCall("Exec", &sandboxv1.ExecResponse{})
				call.StartOffset = test.offset
				plan.Calls, plan.FinalWait = []replay.Call{call}, finalWait
				client := scheduledReplayClient{exec: func(ctx context.Context) (*sandboxv1.ExecResponse, error) {
					if elapsed := time.Since(started); elapsed != test.want {
						t.Errorf("%s: launch after %s, want %s", test.name, elapsed, test.want)
					}
					deadline, bounded := ctx.Deadline()
					if !bounded || time.Until(deadline) != callTimeout {
						t.Errorf("%s: RPC allowance = %s, bounded=%t", test.name, time.Until(deadline), bounded)
					}
					return &sandboxv1.ExecResponse{}, nil
				}}
				ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline+test.ready+test.offset)
				defer cancel()
				<-time.After(test.ready)
				if err := ReplayCalls(ctx, callTimeout, client, plan); err != nil {
					t.Error(err)
				}
				if elapsed := time.Since(started); elapsed != test.want+finalWait {
					t.Errorf("%s: return after %s, want %s", test.name, elapsed, test.want+finalWait)
				}
			}()
		}
		for range tests {
			<-done
		}
	})
}

func TestReplayCallsCancelSharedFutureSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const step = time.Second
		origin := time.Now().Add(step + step)
		ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
		defer cancel()
		go func() {
			<-time.After(step)
			cancel()
		}()
		client := scheduledReplayClient{exec: func(context.Context) (*sandboxv1.ExecResponse, error) {
			t.Error("call launched after cancellation")
			return &sandboxv1.ExecResponse{}, nil
		}}
		call := expectedReplayCall("Exec", &sandboxv1.ExecResponse{})
		if err := ReplayCalls(ctx, step, client, replay.Plan{ReplayAt: &origin, Calls: []replay.Call{call}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("shared schedule cancellation = %v", err)
		}
	})
}

func TestReplayCallsRequireDeadlineAndSkipEmptyTrace(t *testing.T) {
	if err := ReplayCalls(context.Background(), replayTestDeadline, nil, replay.Plan{Calls: []replay.Call{{Operation: "Exec"}}}); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("missing deadline accepted: %v", err)
	}
	zero := time.Time{}
	future := time.Now().Add(replayTestDeadline)
	for _, origin := range []*time.Time{nil, &zero, &future} {
		if err := ReplayCalls(context.Background(), replayTestDeadline, nil, replay.Plan{ReplayAt: origin, FinalWait: replayTestDeadline}); err != nil {
			t.Fatalf("empty trace waited or required client: %v", err)
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-replayTestDeadline))
	defer cancel()
	if err := ReplayCalls(ctx, replayTestDeadline, nil, replay.Plan{Calls: []replay.Call{{Operation: "Exec"}}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deadline accepted: %v", err)
	}
}
