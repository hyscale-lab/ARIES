package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

const lockedUsername = "aries"

// Main implements the exact invocation ARIES renders into Codex's
// environments.toml: --address HOST:PORT. It returns a process exit code.
func Main(ctx context.Context, args []string, stdin io.ReadCloser, stdout, stderr io.Writer) int {
	address, err := parseArguments(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-codex-ssh: %v\n", err)
		return 255
	}
	opener := &SSHOpener{Stderr: stderr, Dial: func(ctx context.Context) (*gossh.Client, func() error, error) {
		return dial(ctx, address)
	}}
	// Cancellation ends Codex's input and every channel, so Serve returns.
	stop := context.AfterFunc(ctx, func() { _ = stdin.Close(); _ = opener.Close() })
	defer stop()
	err = NewRouter(ctx, opener, stdout, stderr).Serve(stdin)
	_ = opener.Close()
	if err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(stderr, "aries-codex-ssh: read Codex input: %v\n", err)
		return 255
	}
	return 0
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
