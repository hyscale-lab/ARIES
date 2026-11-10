package grpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/replay"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ReplayCalls sends a validated plan through a ready, private bridge client.
// It does not return while any started call can still use the bridge.
func ReplayCalls(ctx context.Context, callTimeout time.Duration, client sandboxv1.SandboxClient, plan replay.Plan) error {
	if len(plan.Calls) == 0 {
		return ctx.Err()
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return errors.New("replay caller requires a deadline")
	}
	callsCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(plan.Calls))
	active := 0
	var failure error
	var cleanup *time.Timer
	var cleanupDone <-chan time.Time
	ctxDone := ctx.Done()
	defer func() {
		if cleanup != nil {
			cleanup.Stop()
		}
	}()
	fail := func(err error) {
		if err != nil && failure == nil {
			failure = err
			cancel()
			ctxDone = nil
			cleanup = time.NewTimer(defaultBridgeCleanup)
			cleanupDone = cleanup.C
		}
	}
	collect := func(err error) {
		active--
		fail(err)
	}
	origin := time.Now()
	if plan.ReplayAt != nil {
		// Map the cross-process wall time to this process's monotonic clock once.
		origin = origin.Add(plan.ReplayAt.Round(0).Sub(origin.Round(0)))
	}
launch:
	for _, call := range plan.Calls {
		for failure == nil {
			wait := time.Until(origin.Add(call.StartOffset))
			if wait <= 0 {
				break
			}
			timer := time.NewTimer(wait)
			select {
			case err := <-results:
				collect(err)
			case <-ctxDone:
				fail(ctx.Err())
			case <-timer.C:
			}
			timer.Stop()
		}
		// Collect completed calls before an overdue launch.
		for failure == nil && active > 0 {
			select {
			case err := <-results:
				collect(err)
			default:
				goto ready
			}
		}
	ready:
		if failure != nil {
			break launch
		}
		if err := ctx.Err(); err != nil {
			fail(err)
			break launch
		}
		active++
		callCtx, done := context.WithTimeout(callsCtx, callTimeout)
		go func() {
			defer done()
			results <- replayCall(callCtx, client, call)
		}()
	}
	for active > 0 {
		select {
		case err := <-results:
			collect(err)
		case <-ctxDone:
			fail(ctx.Err())
		case <-cleanupDone:
			return errors.Join(failure, errors.New("replay caller termination is unconfirmed"))
		}
	}
	if failure != nil {
		return failure
	}
	if plan.FinalWait > 0 {
		timer := time.NewTimer(plan.FinalWait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return ctx.Err()
}

func replayCall(ctx context.Context, client sandboxv1.SandboxClient, call replay.Call) error {
	if call.Sequence != 0 {
		ctx = metadata.AppendToOutgoingContext(ctx, replaySourceSequenceKey, strconv.FormatUint(call.Sequence, replaySequenceBase))
	}
	if call.ToolCallID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, toolCallIDKey, call.ToolCallID)
	}
	var outcome *responseOutcome
	var err error
	var written *sandboxv1.WriteFileResponse
	var exitCodeChanged bool
	switch call.Operation {
	case "Exec":
		outcome = newResponseOutcome(sandboxv1.Sandbox_Exec_FullMethodName)
		response, callErr := client.Exec(ctx, &sandboxv1.ExecRequest{Script: call.Script, Stdin: call.Body, Login: call.Login})
		err = callErr
		if err == nil {
			outcome.message(response)
			exitCodeChanged = response.GetExitCode() != call.ExitCode
		}
	case "Stat":
		outcome = newResponseOutcome(sandboxv1.Sandbox_Stat_FullMethodName)
		response, callErr := client.Stat(ctx, &sandboxv1.StatRequest{Path: call.Path})
		err = callErr
		if err == nil {
			outcome.message(response)
		}
	case "ReadFile":
		outcome = newResponseOutcome(sandboxv1.Sandbox_ReadFile_FullMethodName)
		var stream sandboxv1.Sandbox_ReadFileClient
		stream, err = client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: call.Path, MaxBytes: call.MaxBytes})
		if err == nil {
			chunks := &chunkWriter{send: func(body []byte) error {
				outcome.message(&sandboxv1.ReadFileResponse{Part: &sandboxv1.ReadFileResponse_Chunk{Chunk: body}})
				return nil
			}}
			for {
				response, recvErr := stream.Recv()
				if recvErr != nil {
					if recvErr != io.EOF {
						err = recvErr
					}
					_ = chunks.flush()
					break
				}
				if response.GetHeader() != nil {
					_ = chunks.flush()
					outcome.message(response)
				} else if response.GetPart() != nil {
					_, _ = chunks.Write(response.GetChunk())
				}
			}
		}
	case "ReadLines":
		outcome = newResponseOutcome(sandboxv1.Sandbox_ReadLines_FullMethodName)
		var stream sandboxv1.Sandbox_ReadLinesClient
		stream, err = client.ReadLines(ctx, &sandboxv1.ReadLinesRequest{Path: call.Path, FirstLine: call.FirstLine, MaxLines: call.MaxLines, MaxLineBytes: call.MaxLineBytes})
		if err == nil {
			chunks := &chunkWriter{send: func(body []byte) error {
				outcome.message(&sandboxv1.ReadLinesResponse{Part: &sandboxv1.ReadLinesResponse_Chunk{Chunk: body}})
				return nil
			}}
			for {
				response, recvErr := stream.Recv()
				if recvErr != nil {
					if recvErr != io.EOF {
						err = recvErr
					}
					_ = chunks.flush()
					break
				}
				if response.GetSummary() != nil {
					_ = chunks.flush()
					outcome.message(response)
				} else if response.GetPart() != nil {
					_, _ = chunks.Write(response.GetChunk())
				}
			}
		}
	case "WriteFile":
		outcome = newResponseOutcome(sandboxv1.Sandbox_WriteFile_FullMethodName)
		var stream sandboxv1.Sandbox_WriteFileClient
		stream, err = client.WriteFile(ctx)
		if err == nil {
			err = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Header{Header: &sandboxv1.WriteFileHeader{Path: call.Path, Size: int64(len(call.Body))}}})
		}
		for offset := 0; err == nil && offset < len(call.Body); offset += chunkBytes {
			end := min(offset+chunkBytes, len(call.Body))
			err = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Chunk{Chunk: call.Body[offset:end]}})
		}
		if err == nil {
			written, err = stream.CloseAndRecv()
			if err == nil {
				outcome.message(written)
			}
		}
	default:
		return fmt.Errorf("replay call %d has unknown operation", call.Sequence)
	}
	outcome.field([]byte(status.Code(err).String()))
	outcome.field([]byte(status.Convert(err).Message()))
	if outcome.invalid || outcome.bytes != call.ResponseBytes || status.Code(err).String() != call.ResponseCode || exitCodeChanged || written != nil && written.GetBytesWritten() != int64(len(call.Body)) {
		return fmt.Errorf("replay call %d %s response mismatch", call.Sequence, call.Operation)
	}
	return nil
}
