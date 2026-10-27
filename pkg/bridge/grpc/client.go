package grpc

// The client staged into the harness container, where the ARIES Hermes plugin
// runs it by path once per operation:
//
//	aries-grpc-client exec [--login] [--] SCRIPT
//	aries-grpc-client file stat PATH
//	aries-grpc-client file read [--max-bytes N] PATH
//	aries-grpc-client file lines --first N --max N [--max-line-bytes N] PATH
//	aries-grpc-client file write --size N PATH < content
//
// It is deliberately thin. exec sends SCRIPT, stdin and the login flag as a
// typed call; the bridge decides how the script runs. file forwards a path and
// raw bytes; how they land is the bridge's and the sandbox's business. For
// file, stdout is the payload only and stderr is exactly one line: JSON
// metadata on success, a message on failure. Content streams through in chunks
// and is never held whole here; a failure after some content leaves it on
// stdout and exits non-zero, so the caller must check the exit code before
// using stdout. There is no timeout flag: Hermes kills the process when a
// command times out, and the closed connection cancels the call.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// targetEnv names the bridge address, which the harness sets in the
// environment rather than in argv.
const targetEnv = "ARIES_GRPC_TARGET"

// toolCallIDEnv names the harness tool call that this operation serves. The
// plugin sets it per operation; the client sends it to the bridge as is.
const toolCallIDEnv = "ARIES_TOOL_CALL_ID"

// transportFailureExit means the operation did not run at all: a usage error,
// a transport failure, or a bridge refusal. It is OpenSSH's value for the same
// case, so for exec it cannot be mistaken for a code the command produced
// unless the command itself exits 255.
const transportFailureExit = 255

// fileExit maps a file procedure's status to the client's exit code. The
// values are disjoint from anything a remote command returns because file
// never runs one.
var fileExit = map[codes.Code]int{
	codes.OK: 0, codes.NotFound: 1, codes.PermissionDenied: 2, codes.InvalidArgument: 3,
	codes.ResourceExhausted: 4, codes.FailedPrecondition: 5,
}

const clientUsage = `usage: aries-grpc-client exec [--login] [--] SCRIPT
       aries-grpc-client file stat|read|lines|write [flags] PATH`

// ClientMain runs one operation through the bridge and returns the exit code
// the caller should exit with.
func ClientMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "exec" {
		return execMain(args[1:], stdin, stdout, stderr)
	}
	if len(args) > 1 && args[0] == "file" {
		return fileMain(args[1], args[2:], stdin, stdout, stderr)
	}
	fmt.Fprintln(stderr, clientUsage)
	return transportFailureExit
}

func execMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	login := flags.Bool("login", false, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 {
		fmt.Fprintln(stderr, clientUsage)
		return transportFailureExit
	}
	input, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "aries: read stdin: %v\n", err)
		return transportFailureExit
	}
	client, done, err := connect()
	if err != nil {
		fmt.Fprintf(stderr, "aries: %v\n", err)
		return transportFailureExit
	}
	defer done()

	response, err := client.Exec(outgoingContext(), &sandboxv1.ExecRequest{Script: flags.Arg(0), Stdin: input, Login: *login})
	if err != nil {
		// The command did not run, and the caller must not read this as an
		// exit code the command produced.
		fmt.Fprintf(stderr, "aries: %v\n", err)
		return transportFailureExit
	}
	if _, err := stdout.Write(response.GetStdout()); err != nil {
		fmt.Fprintf(stderr, "aries: write stdout: %v\n", err)
		return transportFailureExit
	}
	if _, err := stderr.Write(response.GetStderr()); err != nil {
		return transportFailureExit
	}
	if response.GetTruncated() {
		fmt.Fprintln(stderr, "aries: output truncated at the bridge's limit")
	}
	// The bridge then reports exit 255 too; the reason tells it apart from a
	// command that exits 255.
	if reason := response.GetReason(); reason != sandboxv1.Reason_REASON_COMPLETED {
		fmt.Fprintf(stderr, "aries: %s\n", reason)
	}
	return int(response.GetExitCode())
}

func fileMain(operation string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var maxBytes, first, count, lineBytes, size *int64
	switch operation {
	case "read":
		maxBytes = flags.Int64("max-bytes", 0, "")
	case "lines":
		first, count, lineBytes = flags.Int64("first", 0, ""), flags.Int64("max", 0, ""), flags.Int64("max-line-bytes", 0, "")
	case "write":
		size = flags.Int64("size", -1, "")
	case "stat":
	default:
		fmt.Fprintln(stderr, clientUsage)
		return transportFailureExit
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 || (size != nil && *size < 0) {
		fmt.Fprintln(stderr, clientUsage)
		return transportFailureExit
	}
	path := flags.Arg(0)
	client, done, err := connect()
	if err != nil {
		fmt.Fprintf(stderr, "aries: %v\n", err)
		return transportFailureExit
	}
	defer done()

	ctx := outgoingContext()
	var metadata any
	switch operation {
	case "stat":
		var response *sandboxv1.StatResponse
		if response, err = client.Stat(ctx, &sandboxv1.StatRequest{Path: path}); err == nil {
			// stat reports on stdout because the report is its payload.
			types := map[sandboxv1.FileType]string{sandboxv1.FileType_FILE_TYPE_REGULAR: "regular", sandboxv1.FileType_FILE_TYPE_DIRECTORY: "directory", sandboxv1.FileType_FILE_TYPE_OTHER: "other"}
			report, _ := json.Marshal(map[string]any{"exists": response.GetExists(), "type": types[response.GetType()], "size": response.GetSize(), "mode": fmt.Sprintf("%04o", response.GetMode())})
			_, err = fmt.Fprintf(stdout, "%s\n", report)
		}
	case "read":
		metadata, err = receiveRead(ctx, client, &sandboxv1.ReadFileRequest{Path: path, MaxBytes: *maxBytes}, stdout)
	case "lines":
		metadata, err = receiveLines(ctx, client, &sandboxv1.ReadLinesRequest{Path: path, FirstLine: *first, MaxLines: *count, MaxLineBytes: *lineBytes}, stdout)
	case "write":
		metadata, err = sendWrite(ctx, client, &sandboxv1.WriteFileHeader{Path: path, Size: *size}, stdin)
	}
	if err != nil {
		fmt.Fprintf(stderr, "aries: %s\n", status.Convert(err).Message())
		if code, known := fileExit[status.Code(err)]; known {
			return code
		}
		return transportFailureExit
	}
	if metadata != nil {
		line, _ := json.Marshal(metadata)
		fmt.Fprintf(stderr, "%s\n", line)
	}
	return 0
}

// receiveRead copies the chunks of a ReadFile stream to stdout and returns the
// header as metadata.
func receiveRead(ctx context.Context, client sandboxv1.SandboxClient, request *sandboxv1.ReadFileRequest, stdout io.Writer) (any, error) {
	stream, err := client.ReadFile(ctx, request)
	if err != nil {
		return nil, err
	}
	var header *sandboxv1.ReadFileHeader
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if message.GetHeader() != nil {
			header = message.GetHeader()
		} else if _, err := stdout.Write(message.GetChunk()); err != nil {
			return nil, err
		}
	}
	if header == nil {
		return nil, errors.New("stream ended without a header")
	}
	return map[string]any{"size": header.GetSize(), "truncated": header.GetTruncated()}, nil
}

// receiveLines copies the window of a ReadLines stream to stdout and returns
// the summary, which arrives last, as metadata.
func receiveLines(ctx context.Context, client sandboxv1.SandboxClient, request *sandboxv1.ReadLinesRequest, stdout io.Writer) (any, error) {
	stream, err := client.ReadLines(ctx, request)
	if err != nil {
		return nil, err
	}
	var summary *sandboxv1.ReadLinesSummary
	for {
		message, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if message.GetSummary() != nil {
			summary = message.GetSummary()
		} else if _, err := stdout.Write(message.GetChunk()); err != nil {
			return nil, err
		}
	}
	if summary == nil {
		return nil, errors.New("stream ended without a summary")
	}
	return map[string]any{"total_lines": summary.GetTotalLines(), "size": summary.GetSize(), "ends_with_newline": summary.GetEndsWithNewline(), "more": summary.GetMore()}, nil
}

// sendWrite streams stdin to WriteFile after the header. The bridge checks
// that exactly header.size bytes arrive, so a short or long stdin fails there
// and the file is left as it was.
func sendWrite(ctx context.Context, client sandboxv1.SandboxClient, header *sandboxv1.WriteFileHeader, stdin io.Reader) (any, error) {
	stream, err := client.WriteFile(ctx)
	if err != nil {
		return nil, err
	}
	err = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Header{Header: header}})
	for err == nil {
		// A fresh buffer per message: gRPC may still hold a sent one.
		chunk := make([]byte, chunkBytes)
		read, readErr := io.ReadFull(stdin, chunk)
		if read > 0 {
			err = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Chunk{Chunk: chunk[:read]}})
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read stdin: %w", readErr)
		}
	}
	// A failed Send means the bridge ended the call; CloseAndRecv reports why.
	response, err := stream.CloseAndRecv()
	if err != nil {
		return nil, err
	}
	return map[string]any{"bytes_written": response.GetBytesWritten(), "created": response.GetCreated()}, nil
}

// outgoingContext carries the tool call id from the environment, if any, to
// the bridge. The bridge, not the client, checks the value.
func outgoingContext() context.Context {
	ctx := context.Background()
	if id := os.Getenv(toolCallIDEnv); id != "" {
		ctx = grpcmetadata.AppendToOutgoingContext(ctx, toolCallIDKey, id)
	}
	return ctx
}

// connect dials the bridge named by the harness environment.
func connect() (sandboxv1.SandboxClient, func(), error) {
	target := os.Getenv(targetEnv)
	if strings.TrimSpace(target) == "" {
		return nil, nil, fmt.Errorf("%s is not set", targetEnv)
	}
	connection, err := grpc.NewClient(target,
		// The bridge is plaintext and unauthenticated.
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		// grpc-go honours HTTPS_PROXY by default. The bridge listens on the
		// run network, which a proxy outside that network cannot reach, and
		// the address changes per session, so NO_PROXY in the harness image
		// cannot be relied on to exempt it.
		grpc.WithNoProxy(),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMessageBytes),
			grpc.MaxCallSendMsgSize(maxMessageBytes),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("connect: %w", err)
	}
	return sandboxv1.NewSandboxClient(connection), func() { _ = connection.Close() }, nil
}
