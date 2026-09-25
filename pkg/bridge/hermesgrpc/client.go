package hermesgrpc

// The client staged into the harness container, where the ARIES Hermes plugin
// runs it by path once per command:
//
//	aries-grpc exec [--login] [--] SCRIPT
//
// It is deliberately thin. It wraps SCRIPT as the `bash -c` payload the bridge
// accepts and makes one call; it does not decode Hermes's grammar, because that
// check is a policy gate and belongs on the server, where a caller holding
// these credentials cannot route around it. There is no timeout flag: Hermes
// kills the process when a command times out, and the closed connection
// cancels the call on the bridge.

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Environment variables the harness sets. The target and both credential paths
// come from here rather than from argv.
const (
	targetEnv   = "ARIES_GRPC_TARGET"
	identityEnv = "ARIES_GRPC_IDENTITY"
	trustedEnv  = "ARIES_GRPC_TRUSTED"
)

// transportFailureExit means the command did not run at all: a usage error, a
// transport failure, or a bridge refusal. It is OpenSSH's value for the same
// case, so it cannot be mistaken for a code the command produced unless the
// command itself exits 255.
const transportFailureExit = 255

const clientUsage = "usage: aries-grpc exec [--login] [--] SCRIPT"

// ClientMain runs one command through the bridge and returns the exit code the
// caller should exit with.
func ClientMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "exec" {
		fmt.Fprintln(stderr, clientUsage)
		return transportFailureExit
	}
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	login := flags.Bool("login", false, "")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 1 {
		fmt.Fprintln(stderr, clientUsage)
		return transportFailureExit
	}
	payload := remoteShell + " -c " + shlexQuote(flags.Arg(0))
	if *login {
		payload = remoteShell + " -l -c " + shlexQuote(flags.Arg(0))
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
		fmt.Fprintf(stderr, "aries: connect: %v\n", err)
		return transportFailureExit
	}
	defer func() { _ = connection.Close() }()

	response, err := sandboxv1.NewSandboxClient(connection).Exec(context.Background(),
		&sandboxv1.ExecRequest{Script: payload, Stdin: input})
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
