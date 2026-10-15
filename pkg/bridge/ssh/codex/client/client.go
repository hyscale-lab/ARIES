// Package client is the stdio program Codex's native remote environment
// spawns. Codex 0.157.1 speaks its exec-server JSON-RPC over the program's
// stdin and stdout; this helper carries those bytes over one SSH exec request
// to the bridge and interprets none of them.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	sshclient "github.com/hyscale-lab/aries/pkg/bridge/ssh/client"
)

// RemoteCommand is the only exec request the Codex dialect accepts. It names
// the native executor rather than a shell command.
const RemoteCommand = "aries-codex-exec-server-v1"

const lockedUsername = "aries"

// Main implements the exact invocation ARIES renders into Codex's
// environments.toml: --address HOST:PORT. It returns a process exit code.
func Main(ctx context.Context, args []string, stdin io.ReadCloser, stdout, stderr io.Writer) int {
	address, err := parseArguments(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-codex-ssh: %v\n", err)
		return 255
	}
	code, err := sshclient.Run(ctx, sshclient.Config{Address: address, User: lockedUsername}, RemoteCommand, stdin, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-codex-ssh: %v\n", err)
		return 255
	}
	return code
}

func parseArguments(args []string) (string, error) {
	if len(args) != 2 || args[0] != "--address" {
		return "", errors.New("expected exactly --address HOST:PORT")
	}
	host, portText, err := net.SplitHostPort(args[1])
	port, portErr := strconv.Atoi(portText)
	if err != nil || host == "" || strings.ContainsAny(host, "\x00 \t\r\n") || portErr != nil || port < 1 || port > 65535 {
		return "", errors.New("address must be HOST:PORT with a port between 1 and 65535")
	}
	return args[1], nil
}
