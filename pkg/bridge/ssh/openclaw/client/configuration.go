package client

import (
	"context"
	"io"
	"net"
	"strconv"

	sshclient "github.com/hyscale-lab/aries/pkg/bridge/ssh/client"
)

func runSSHClient(ctx context.Context, configuration clientConfig, remote string, stdin io.ReadCloser, stdout, stderr io.Writer) (int, error) {
	address := net.JoinHostPort(configuration.hostName, strconv.Itoa(configuration.port))
	return sshclient.Run(ctx, sshclient.Config{Address: address, User: lockedUsername}, remote, stdin, stdout, stderr)
}
