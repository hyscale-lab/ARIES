package hermesgrpc

// The client staged into the harness container, where the ARIES Hermes plugin
// runs it by path once per operation:
//
//	aries-grpc exec [--login] [--] SCRIPT
//	aries-grpc file stat PATH
//	aries-grpc file read [--offset N] [--max-bytes N] PATH
//	aries-grpc file lines --first N --max N [--max-line-bytes N] PATH
//	aries-grpc file write PATH < content
//
// It is deliberately thin. exec wraps SCRIPT as the `bash -c` payload the
// bridge accepts; it does not decode Hermes's grammar, because that check is a
// policy gate and belongs on the server, where a caller holding these
// credentials cannot route around it. file forwards a path and raw bytes; how
// they land is the bridge's and the sandbox's business. For file, stdout is the
// payload only and stderr is exactly one line: JSON metadata on success, a
// message on failure. There is no timeout flag: Hermes kills the process when
// a command times out, and the closed connection cancels the call.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Environment variables the harness sets. The target and both credential paths
// come from here rather than from argv.
const (
	targetEnv   = "ARIES_GRPC_TARGET"
	identityEnv = "ARIES_GRPC_IDENTITY"
	trustedEnv  = "ARIES_GRPC_TRUSTED"
)

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

const clientUsage = `usage: aries-grpc exec [--login] [--] SCRIPT
       aries-grpc file stat|read|lines|write [flags] PATH`

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
	payload := remoteShell + " -c " + shlexQuote(flags.Arg(0))
	if *login {
		payload = remoteShell + " -l -c " + shlexQuote(flags.Arg(0))
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

	response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: payload, Stdin: input})
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
	return int(response.GetExitCode())
}

func fileMain(operation string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(operation, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var offset, maxBytes, first, count, lineBytes *int64
	switch operation {
	case "read":
		offset, maxBytes = flags.Int64("offset", 0, ""), flags.Int64("max-bytes", 0, "")
	case "lines":
		first, count, lineBytes = flags.Int64("first", 0, ""), flags.Int64("max", 0, ""), flags.Int64("max-line-bytes", 0, "")
	case "stat", "write":
	default:
		fmt.Fprintln(stderr, clientUsage)
		return transportFailureExit
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 1 {
		fmt.Fprintln(stderr, clientUsage)
		return transportFailureExit
	}
	path := flags.Arg(0)
	var content []byte
	if operation == "write" {
		var err error
		if content, err = io.ReadAll(stdin); err != nil {
			fmt.Fprintf(stderr, "aries: read stdin: %v\n", err)
			return transportFailureExit
		}
	}
	client, done, err := connect()
	if err != nil {
		fmt.Fprintf(stderr, "aries: %v\n", err)
		return transportFailureExit
	}
	defer done()

	ctx := context.Background()
	var payload []byte
	var metadata any
	switch operation {
	case "stat":
		var response *sandboxv1.StatResponse
		if response, err = client.Stat(ctx, &sandboxv1.StatRequest{Path: path}); err == nil {
			// stat reports on stdout because the report is its payload.
			types := map[sandboxv1.FileType]string{sandboxv1.FileType_FILE_TYPE_REGULAR: "regular", sandboxv1.FileType_FILE_TYPE_DIRECTORY: "directory", sandboxv1.FileType_FILE_TYPE_OTHER: "other"}
			payload, _ = json.Marshal(map[string]any{"exists": response.GetExists(), "type": types[response.GetType()], "size": response.GetSize(), "mode": fmt.Sprintf("%04o", response.GetMode())})
			payload = append(payload, '\n')
		}
	case "read":
		var response *sandboxv1.ReadFileResponse
		if response, err = client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: path, Offset: *offset, MaxBytes: *maxBytes}); err == nil {
			payload, metadata = response.GetContent(), map[string]any{"size": response.GetSize(), "truncated": response.GetTruncated()}
		}
	case "lines":
		var response *sandboxv1.ReadLinesResponse
		if response, err = client.ReadLines(ctx, &sandboxv1.ReadLinesRequest{Path: path, FirstLine: *first, MaxLines: *count, MaxLineBytes: *lineBytes}); err == nil {
			payload, metadata = response.GetContent(), map[string]any{"total_lines": response.GetTotalLines(), "size": response.GetSize(), "ends_with_newline": response.GetEndsWithNewline(), "more": response.GetMore()}
		}
	case "write":
		var response *sandboxv1.WriteFileResponse
		if response, err = client.WriteFile(ctx, &sandboxv1.WriteFileRequest{Path: path, Content: content}); err == nil {
			metadata = map[string]any{"bytes_written": response.GetBytesWritten(), "created": response.GetCreated()}
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "aries: %s\n", status.Convert(err).Message())
		if code, known := fileExit[status.Code(err)]; known {
			return code
		}
		return transportFailureExit
	}
	if _, err := stdout.Write(payload); err != nil {
		return transportFailureExit
	}
	if metadata != nil {
		line, _ := json.Marshal(metadata)
		fmt.Fprintf(stderr, "%s\n", line)
	}
	return 0
}

// connect dials the bridge named by the harness environment.
func connect() (sandboxv1.SandboxClient, func(), error) {
	target := os.Getenv(targetEnv)
	if strings.TrimSpace(target) == "" {
		return nil, nil, fmt.Errorf("%s is not set", targetEnv)
	}
	transport, err := clientCredentials(os.Getenv(identityEnv), os.Getenv(trustedEnv))
	if err != nil {
		return nil, nil, err
	}
	connection, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(transport),
		// grpc-go honours HTTPS_PROXY by default. The bridge is reachable only
		// on the task network and a proxy would carry every script, its stdin
		// and all output off that network, so proxying is refused outright
		// rather than left to whatever the harness image's environment says.
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

func clientCredentials(identityPath, trustedPath string) (credentials.TransportCredentials, error) {
	if strings.TrimSpace(identityPath) == "" || strings.TrimSpace(trustedPath) == "" {
		return nil, fmt.Errorf("%s and %s must both be set", identityEnv, trustedEnv)
	}
	identity, err := tls.LoadX509KeyPair(identityPath, identityPath)
	if err != nil {
		return nil, fmt.Errorf("load client identity: %w", err)
	}
	trustedPEM, err := os.ReadFile(trustedPath)
	if err != nil {
		return nil, fmt.Errorf("read trusted certificate: %w", err)
	}
	trusted, err := parseCertificate(trustedPEM)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{identity},
		MinVersion:   tls.VersionTLS13,
		// The bridge's certificate is self-signed and pinned by raw bytes, so
		// chain verification is replaced rather than skipped. Exactly one
		// server is acceptable.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: pinnedPeer(trusted),
	}), nil
}
