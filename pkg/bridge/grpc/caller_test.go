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
	"testing/fstest"
	"testing/synctest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/sirupsen/logrus"
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
		call.ExitCode = new(messages[0].(*sandboxv1.ExecResponse).GetExitCode())
	}
	return call
}

func TestReplayCallsRejectsUnsupportedOriginBeforeRPC(t *testing.T) {
	response := &sandboxv1.ExecResponse{}
	call := expectedReplayCall("Exec", response)
	var called atomic.Bool
	client := replayEndpoint(t, &replayServer{exec: func(context.Context, *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
		called.Store(true)
		return response, nil
	}})
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	for _, origin := range []string{replay.OriginHarbor, "unknown"} {
		plan := replay.Plan{OriginType: origin, Calls: []replay.Call{call}}
		if err := ReplayCalls(ctx, replayTestDeadline, client, plan); err == nil {
			t.Errorf("unsupported origin %q was accepted", origin)
		}
	}
	if called.Load() {
		t.Error("unsupported origin sent an RPC")
	}
}

func TestReplayHarborExec(t *testing.T) {
	const privateText = "é\n"
	const changedExit = 7
	const privateRPC = "private transport text"
	for _, tc := range []struct {
		name, source, stdout, stderr, want string
		sourceCut, targetCut, rpcError     bool
		exit                               int32
	}{
		{name: "UTF-8 and newline", source: privateText, stdout: privateText},
		{name: "empty", source: ""},
		{name: "newline missing", source: privateText, stdout: "é", want: "output length mismatch"},
		{name: "content drift", source: privateText, stdout: "aé"},
		{name: "unknown interleaving", source: privateText, stdout: "\n", stderr: "é"},
		{name: "unknown interleaving length mismatch", source: privateText, stdout: "\n", stderr: "a", want: "output length mismatch"},
		{name: "source truncated", source: "é", stdout: privateText, sourceCut: true},
		{name: "target truncated", source: privateText, stdout: "é", targetCut: true},
		{name: "both truncated exit mismatch", source: privateText, stdout: "é", sourceCut: true, targetCut: true, exit: changedExit, want: "exit code mismatch"},
		{name: "exit mismatch", source: privateText, stdout: privateText, exit: changedExit, want: "exit code mismatch"},
		{name: "transport error", source: privateText, rpcError: true, want: "RPC failed (Unavailable)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), replayTestDeadline)
			defer cancel()
			call := replay.Call{Operation: "Exec", HarborOutput: &tc.source, HarborTruncated: tc.sourceCut, ExitCode: new(int32)}
			server := &replayServer{exec: func(context.Context, *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
				if tc.rpcError {
					return nil, status.Error(codes.Unavailable, privateRPC)
				}
				return &sandboxv1.ExecResponse{Stdout: []byte(tc.stdout), Stderr: []byte(tc.stderr), Truncated: tc.targetCut, ExitCode: tc.exit, Reason: sandboxv1.Reason_REASON_COMPLETED}, nil
			}}
			err := ReplayCalls(ctx, replayTestDeadline, replayEndpoint(t, server), replay.Plan{OriginType: replay.OriginHarbor, Calls: []replay.Call{call}})
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), privateRPC)) {
				t.Fatalf("caller error = %v, want %q", err, tc.want)
			}
		})
	}
}

func harborTestWrite(t *testing.T, path, body string, count int64, rpcError bool, privateRPC string) func(grpc.ClientStreamingServer[sandboxv1.WriteFileRequest, sandboxv1.WriteFileResponse]) error {
	t.Helper()
	return func(stream grpc.ClientStreamingServer[sandboxv1.WriteFileRequest, sandboxv1.WriteFileResponse]) error {
		first, err := stream.Recv()
		if err != nil {
			return err
		}
		if header := first.GetHeader(); header.GetPath() != path || header.GetSize() != int64(len(body)) {
			t.Error("WriteFile header differs from converted source")
		}
		var got []byte
		for {
			req, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			got = append(got, req.GetChunk()...)
		}
		if string(got) != body {
			t.Error("WriteFile body differs from converted source")
		}
		if rpcError {
			return status.Error(codes.PermissionDenied, privateRPC)
		}
		return stream.SendAndClose(&sandboxv1.WriteFileResponse{BytesWritten: count})
	}
}

func TestReplayHarborFinalizedOutput(t *testing.T) {
	const sequence = 1
	const output = "private-é\n"
	const changed = "private-ø\n"
	const changedExit = 7
	for _, tc := range []struct {
		name, source, stdout, stderr, want string
		truncated                          bool
		exit                               int
	}{
		{name: "exact", source: output, stdout: output, want: "matched"},
		{name: "empty", want: "matched"},
		{name: "drift", source: output, stderr: changed, want: "drifted"},
		{name: "two streams", source: output, stdout: "private-", stderr: "é\n", want: "length_matched"},
		{name: "length mismatch", source: output, stdout: changed + "\n", want: "length_mismatch"},
		{name: "truncated", source: output, stdout: "private-", truncated: true, want: "inconclusive"},
		{name: "truncated with changed exit", source: output, stdout: "private-", truncated: true, exit: changedExit, want: "exit_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := newFakeExecutor(nil)
			target.stdout, target.stderr = []byte(tc.stdout), []byte(tc.stderr)
			target.exitCode = tc.exit
			manager, client, endpoint := startBridge(t, target, Options{CaptureReplay: true, OmitRawLog: true})
			ctx, cancel := context.WithTimeout(t.Context(), replayTestDeadline)
			defer cancel()
			plan := replay.Plan{OriginType: replay.OriginHarbor, Calls: []replay.Call{{
				Operation: "Exec", Sequence: sequence, Script: "true", HarborOutput: &tc.source, HarborTruncated: tc.truncated, ExitCode: new(int32),
			}}}
			err := ReplayCalls(ctx, replayTestDeadline, client, plan)
			if (err != nil) != (tc.want == "length_mismatch" || tc.want == "exit_mismatch") {
				t.Fatalf("unexpected caller outcome: %v", err)
			}
			stop(t, manager)
			var logs bytes.Buffer
			logger := logrus.New()
			logger.SetOutput(&logs)
			results := replay.ReadResponses(endpoint.LogPaths[0], plan, logger)
			data, err := json.Marshal(results)
			if err != nil {
				t.Fatal(err)
			}
			if results.Status != "available" || len(results.Calls) != len(plan.Calls) || results.Calls[0].Status != tc.want {
				t.Fatalf("results = %s, want available %q", data, tc.want)
			}
			if bytes.Contains(data, []byte("private-")) || bytes.Contains(data, []byte("sha256")) || bytes.Contains(logs.Bytes(), []byte("private-")) {
				t.Fatal("Harbor diagnostics contain private evidence")
			}
		})
	}
}

func TestReplayHarborFinalizedWriteWithoutRetainedBody(t *testing.T) {
	const writeSequence = 1
	const execSequence = writeSequence + 1
	const path = "/app/example.txt"
	const body = "private\fbody"
	const output = "kept\n"
	target := newFakeExecutor(make(fstest.MapFS))
	target.stdout = []byte(output)
	manager, client, endpoint := startBridge(t, target, Options{CaptureReplay: true, OmitRawLog: true})
	ctx, cancel := context.WithTimeout(t.Context(), replayTestDeadline)
	defer cancel()
	sourceOutput := output
	plan := replay.Plan{OriginType: replay.OriginHarbor, Calls: []replay.Call{
		{Operation: "WriteFile", Sequence: writeSequence, Path: path, Body: []byte(body)},
		{Operation: "Exec", Sequence: execSequence, Script: "true", HarborOutput: &sourceOutput, ExitCode: new(int32)},
	}}
	if err := ReplayCalls(ctx, replayTestDeadline, client, plan); err != nil {
		t.Fatal(err)
	}
	stop(t, manager)
	for _, record := range readToolCalls(t, endpoint.LogPaths[0]) {
		if record["operation_class"] == kindFileWrite && record["replay_error"] != "write-content-omitted" {
			t.Fatal("test did not produce an omitted write body")
		}
	}
	responses := replay.ReadResponses(endpoint.LogPaths[0], plan, logrus.New())
	if responses.Status != "available" || len(responses.Calls) != len(plan.Calls) {
		t.Fatalf("finalized responses are incomplete: %+v", responses)
	}
	for index, call := range responses.Calls {
		if call.SourceSequence != plan.Calls[index].Sequence || call.Status != "matched" {
			t.Fatalf("unexpected finalized response: %+v", call)
		}
	}
}

func TestReplayHarborSourceRejectionOverlapsDispatchedCalls(t *testing.T) {
	const firstSequence = 1
	const rejectionSequence = firstSequence + 1
	const lastSequence = rejectionSequence + 1
	const dispatched = lastSequence - 1
	const interval = 40 * time.Millisecond
	const action = "/app/example.txt"
	const original = "original content"
	for _, skip := range []bool{false, true} {
		target := newFakeExecutor(fstest.MapFS{"app/example.txt": {Data: []byte(original)}})
		manager, client, endpoint := startBridge(t, target, Options{CaptureReplay: true, OmitRawLog: true})
		plan := replay.Plan{OriginType: replay.OriginHarbor, SkipResponseCheck: skip, Calls: []replay.Call{
			{Sequence: firstSequence, Operation: "Exec", Script: "true", HarborOutput: new(string)},
			{Sequence: rejectionSequence, Operation: "Edit", ToolCallID: "toolu_fixture", StartOffset: interval,
				SourceRejection: &replay.SourceRejection{Reason: replay.RejectionFileNotRead, Duration: interval + interval}},
			{Sequence: lastSequence, Operation: "Exec", Script: "true", HarborOutput: new(string), StartOffset: interval + interval},
		}, FinalWait: interval}
		ctx, cancel := context.WithTimeout(t.Context(), replayTestDeadline)
		start := time.Now()
		err := ReplayCalls(ctx, replayTestDeadline, client, plan)
		cancel()
		if err != nil || time.Since(start) < interval*time.Duration(lastSequence)+plan.FinalWait {
			t.Fatalf("missing the rejection's source return or final wait: %v", err)
		}
		stop(t, manager)
		timing := replay.ReadTiming(endpoint.LogPaths[0], plan)
		if timing.Status != "available" || len(timing.Calls) != dispatched ||
			timing.Calls[0].SourceSequence != firstSequence || timing.Calls[1].SourceSequence != lastSequence {
			t.Fatalf("unexpected bridge starts: %+v", timing)
		}
		if !skip {
			responses := replay.ReadResponses(endpoint.LogPaths[0], plan, logrus.New())
			if responses.Status != "available" || len(responses.Calls) != dispatched ||
				responses.Calls[0].SourceSequence != firstSequence || responses.Calls[1].SourceSequence != lastSequence {
				t.Fatalf("unexpected bridge responses: %+v", responses)
			}
		}
		file := target.files[fsName(action)]
		if file == nil || string(file.Data) != original || target.fileOps != 0 || len(target.commands) != dispatched {
			t.Fatal("replay changed the file or made an unexpected sandbox call")
		}
	}
}

func TestReplayUncheckedKeepsRPCAndWriteFailures(t *testing.T) {
	const path = "/app/example.txt"
	const body = "synthetic"
	const privateDetail = "private detail"
	for _, tc := range []struct {
		name, operation, want string
		code                  codes.Code
		reason                sandboxv1.Reason
		cancel                error
	}{
		{name: "transport", operation: "Stat", code: codes.Unavailable, want: "RPC failed (Unavailable)"},
		{name: "cancellation", operation: "Exec", code: codes.Canceled, cancel: context.Canceled, want: "RPC failed"},
		{name: "execution timeout", operation: "Exec", reason: sandboxv1.Reason_REASON_TIMED_OUT, cancel: context.DeadlineExceeded, want: "Exec did not complete"},
		{name: "short write", operation: "WriteFile", want: "write length mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), replayTestDeadline)
			defer cancel()
			server := &replayServer{
				stat: func(context.Context, *sandboxv1.StatRequest) (*sandboxv1.StatResponse, error) {
					return nil, status.Error(tc.code, privateDetail)
				},
				exec: func(context.Context, *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
					if tc.code != codes.OK {
						return nil, status.Error(tc.code, privateDetail)
					}
					return &sandboxv1.ExecResponse{Reason: tc.reason}, nil
				},
				writeFile: func(stream grpc.ClientStreamingServer[sandboxv1.WriteFileRequest, sandboxv1.WriteFileResponse]) error {
					for {
						if _, err := stream.Recv(); err != nil {
							if err != io.EOF {
								return err
							}
							return stream.SendAndClose(&sandboxv1.WriteFileResponse{})
						}
					}
				},
			}
			plan := replay.Plan{SkipResponseCheck: true,
				Calls: []replay.Call{{Operation: tc.operation, Path: path, Body: []byte(body)}}}
			err := ReplayCalls(ctx, replayTestDeadline, replayEndpoint(t, server), plan)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), privateDetail) ||
				tc.cancel != nil && !errors.Is(err, tc.cancel) {
				t.Fatalf("unchecked error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestReplayLegacySerializedZeroExit(t *testing.T) {
	const script = "true"
	const first = 0
	const callCount = 1
	for _, tc := range []struct{ name, origin string }{
		{name: "native"},
		{name: "Harbor", origin: replay.OriginHarbor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reply := &sandboxv1.ExecResponse{Reason: sandboxv1.Reason_REASON_COMPLETED}
			call := expectedReplayCall("Exec", reply)
			if tc.origin == replay.OriginHarbor {
				call = replay.Call{Operation: "Exec", HarborOutput: new(string), ExitCode: new(int32)}
			}
			call.Script = script
			encoded, err := replay.EncodePlan(replay.Plan{OriginType: tc.origin, Calls: []replay.Call{call}}, replay.MaxPlanBytes)
			if err != nil {
				t.Fatal(err)
			}
			zeroExit := []byte(`,"exit_code":0`)
			if !bytes.Contains(encoded, zeroExit) {
				t.Fatalf("staged plan omitted the explicit zero exit: %s", encoded)
			}
			// The old encoder omitted zero exits from staged plans.
			legacy := bytes.ReplaceAll(encoded, zeroExit, nil)
			var plan replay.Plan
			if err := json.Unmarshal(legacy, &plan); err != nil || len(plan.Calls) != callCount || plan.Calls[first].ExitCode != nil {
				t.Fatalf("legacy plan did not omit the exit: %v", err)
			}
			dispatched := make(chan string, callCount)
			server := &replayServer{exec: func(_ context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
				dispatched <- request.GetScript()
				return reply, nil
			}}
			ctx, cancel := context.WithTimeout(t.Context(), replayTestDeadline)
			defer cancel()
			if err := ReplayCalls(ctx, replayTestDeadline, replayEndpoint(t, server), plan); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-dispatched:
				if got != script {
					t.Fatalf("dispatched %q, want %q", got, script)
				}
			default:
				t.Fatal("replay did not dispatch the staged call")
			}
		})
	}
}

func TestReplayHarborFinalizedOmittedWriteWithFailedReply(t *testing.T) {
	const sequence = 1
	const path = "/app/example.txt"
	const body = "private\fbody"
	target := newFakeExecutor(make(fstest.MapFS))
	manager, _, endpoint := startBridge(t, target, Options{CaptureReplay: true, OmitRawLog: true})
	stream := &failedWriteReply{
		ctx: metadata.NewIncomingContext(t.Context(), metadata.Pairs(replaySourceSequenceKey, strconv.Itoa(sequence))),
		requests: []*sandboxv1.WriteFileRequest{
			{Part: &sandboxv1.WriteFileRequest_Header{Header: &sandboxv1.WriteFileHeader{Path: path, Size: int64(len(body))}}},
			{Part: &sandboxv1.WriteFileRequest_Chunk{Chunk: []byte(body)}},
		},
	}
	svc := &service{session: manager.active}
	if err := svc.WriteFile(stream); err != nil {
		t.Fatal(err)
	}
	if stream.reply.GetBytesWritten() != int64(len(body)) {
		t.Fatal("bridge did not attempt a full write reply")
	}
	stop(t, manager)
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != sequence || records[0]["status"] != "completed" || records[0]["stdin_encoding"] != "binary-omitted" {
		t.Fatal("test did not produce a completed write with omitted body")
	}
	plan := replay.Plan{OriginType: replay.OriginHarbor, Calls: []replay.Call{
		{Operation: "WriteFile", Sequence: sequence, Path: path, Body: []byte(body)},
	}}
	responses := replay.ReadResponses(endpoint.LogPaths[0], plan, logrus.New())
	if responses.Status != "available" || len(responses.Calls) != len(plan.Calls) || responses.Calls[0].Status != "operation_failed" {
		t.Fatalf("expected finalized response-send failure: %+v", responses)
	}
	if records[0]["replay_error"] != "response-send-failed" {
		t.Fatal("bridge did not retain the response-send marker")
	}
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
	firstCall.Sequence, firstCall.Script, firstCall.ExitCode = sourceSequence, "first", new(source.GetExitCode())
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
	call.ExitCode = new(sourceExitCode)
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

func TestReplayCallsDoNotBlockDispatchDuringSourceRejection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const step = time.Second
		const budget = 6 * step
		started := time.Now()
		origin := started.Add(step)
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		defer cancel()
		client := scheduledReplayClient{exec: func(context.Context) (*sandboxv1.ExecResponse, error) {
			if elapsed := time.Since(origin); elapsed != step {
				t.Errorf("RPC launched at %s, want %s", elapsed, step)
			}
			return &sandboxv1.ExecResponse{Reason: sandboxv1.Reason_REASON_COMPLETED}, nil
		}}
		plan := replay.Plan{OriginType: replay.OriginHarbor, ReplayAt: &origin, FinalWait: step, Calls: []replay.Call{
			{Operation: "Edit", SourceRejection: &replay.SourceRejection{Reason: replay.RejectionFileNotRead, Duration: step + step}},
			{Operation: "Exec", StartOffset: step, HarborOutput: new(string)},
		}}
		if err := ReplayCalls(ctx, step, client, plan); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(origin); elapsed != plan.Calls[0].SourceRejection.Duration+plan.FinalWait {
			t.Fatalf("caller returned at %s, want source return plus final wait", elapsed)
		}
	})
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
