package hermesgrpc

// The client staged into the harness container. Hermes resolves its terminal
// client by name and offers no way to configure one, so this binary is named
// `ssh` inside the container and reached through a PATH entry the harness
// prepends. See docs/design/grpc-bridge.md section 9: that shadowing is a
// workaround, not the end state.
//
// It is deliberately thin. It does not decode Hermes's grammar — that check is
// a policy gate and belongs on the server, where a caller holding these
// credentials cannot route around it. All this does is recover the remote
// command from an OpenSSH-shaped argv, reproduce the payload OpenSSH would have
// put on the wire, and make one call.

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Environment variables the harness sets. The target and both credential paths
// come from here rather than from argv, which keeps the argv coupling to one
// question: which operand is the remote command.
const (
	targetEnv   = "ARIES_GRPC_TARGET"
	identityEnv = "ARIES_GRPC_IDENTITY"
	trustedEnv  = "ARIES_GRPC_TRUSTED"
)

// transportFailureExit is what OpenSSH returns when it cannot run the remote
// command at all. A bridge refusal reaches Hermes as this rather than as a
// command exit code, which is what the SSH bridge produces today when it
// refuses a channel request.
const transportFailureExit = 255

// optionsWithValue are the OpenSSH options that consume the following
// argument. Everything else beginning with '-' is a flag. This is the standard
// ssh(1) option set; the first operand after the options is the destination and
// the remainder is the remote command.
var optionsWithValue = map[byte]bool{
	'b': true, 'c': true, 'D': true, 'E': true, 'e': true, 'F': true, 'I': true,
	'i': true, 'J': true, 'L': true, 'l': true, 'm': true, 'O': true, 'o': true,
	'p': true, 'Q': true, 'R': true, 'S': true, 'W': true, 'w': true,
}

// ClientMain runs one command through the bridge and returns the exit code the
// caller should exit with.
func ClientMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	payload := remotePayload(args)
	if payload == "" {
		// No remote command: this is a connection-setup invocation, which has
		// nothing to run and nothing to report.
		return 0
	}

	target := os.Getenv(targetEnv)
	if strings.TrimSpace(target) == "" {
		fmt.Fprintf(stderr, "%s is not set\n", targetEnv)
		return transportFailureExit
	}
	transport, err := clientCredentials(os.Getenv(identityEnv), os.Getenv(trustedEnv))
	if err != nil {
		fmt.Fprintf(stderr, "aries: %v\n", err)
		return transportFailureExit
	}

	input, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "aries: read stdin: %v\n", err)
		return transportFailureExit
	}

	connection, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(transport),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxMessageBytes),
			grpc.MaxCallSendMsgSize(maxMessageBytes),
		),
	)
	if err != nil {
		fmt.Fprintf(stderr, "aries: connect: %v\n", err)
		return transportFailureExit
	}
	defer func() { _ = connection.Close() }()

	response, err := sandboxv1.NewSandboxClient(connection).Exec(context.Background(),
		&sandboxv1.ExecRequest{Script: payload, Stdin: input})
	if err != nil {
		// A refusal is reported the way a refused SSH channel request is: the
		// command did not run, and the caller must not read this as an exit
		// code the command produced.
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

// remotePayload recovers the remote command from an OpenSSH-shaped argv and
// joins it with single spaces, which is what OpenSSH itself puts on the wire.
// An empty result means the invocation carried no remote command.
func remotePayload(args []string) string {
	index := 0
	for index < len(args) {
		argument := args[index]
		if len(argument) < 2 || argument[0] != '-' {
			break
		}
		if argument == "--" {
			index++
			break
		}
		index++
		if consumesNextArgument(argument) {
			index++
		}
	}
	if index >= len(args) {
		return ""
	}
	// args[index] is the destination; everything after it is the command.
	return strings.Join(args[index+1:], " ")
}

// consumesNextArgument reports whether a short-option cluster takes the
// following argument as its value. Options cluster, and the first one that
// takes a value claims the rest of the cluster if there is any: -p2222 and
// -tp2222 carry the value inline, while -tp takes it from the next argument.
func consumesNextArgument(argument string) bool {
	for index := 1; index < len(argument); index++ {
		if optionsWithValue[argument[index]] {
			return index == len(argument)-1
		}
	}
	return false
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
