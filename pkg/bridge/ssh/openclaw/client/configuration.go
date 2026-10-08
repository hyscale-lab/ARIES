package client

import (
	"context"
	"io"
	"net"
	"strconv"
)

func runSSHClient(ctx context.Context, configuration clientConfig, remote string, stdin io.ReadCloser, stdout, stderr io.Writer) (int, error) {
	address := net.JoinHostPort(configuration.hostName, strconv.Itoa(configuration.port))
	return Run(ctx, Config{Address: address, User: lockedUsername}, remote, stdin, stdout, stderr)
}
