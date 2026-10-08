package client

import (
	"context"
	"errors"
	"fmt"
	gossh "golang.org/x/crypto/ssh"
	"io"
	"net"
	"strconv"
	"strings"
)

func runSSHClient(ctx context.Context, configuration clientConfig, remote string, stdin io.ReadCloser, stdout, stderr io.Writer) (int, error) {
	identity, err := readSecureRegularFile(identityContainerPath, 0o600, maxClientFile)
	if err != nil {
		return 255, fmt.Errorf("read SSH identity: %w", err)
	}
	signer, err := gossh.ParsePrivateKey(identity)
	if err != nil {
		return 255, errors.New("parse SSH identity")
	}
	knownHosts, err := readSecureRegularFile(knownHostsContainerPath, 0o600, maxClientFile)
	if err != nil {
		return 255, fmt.Errorf("read SSH known-hosts file: %w", err)
	}
	hostKey, err := parseLockedKnownHost(knownHosts, configuration.hostName, configuration.port)
	if err != nil {
		return 255, err
	}
	address := net.JoinHostPort(configuration.hostName, strconv.Itoa(configuration.port))
	return Run(ctx, Config{Address: address, User: lockedUsername, Identity: signer, HostKey: hostKey}, remote, stdin, stdout, stderr)
}

func parseLockedKnownHost(content []byte, host string, port int) (gossh.PublicKey, error) {
	line := strings.TrimSuffix(string(content), "\n")
	if line == string(content) || strings.Contains(line, "\n") {
		return nil, errors.New("known-hosts file must contain exactly one newline-terminated entry")
	}
	prefix := "[" + host + "]:" + strconv.Itoa(port) + " "
	if !strings.HasPrefix(line, prefix) {
		return nil, errors.New("known-hosts entry does not match the locked host and port")
	}
	key, comment, options, rest, err := gossh.ParseAuthorizedKey([]byte(strings.TrimPrefix(line, prefix) + "\n"))
	if err != nil || len(rest) != 0 || len(options) != 0 || comment != "" || key.Type() != gossh.KeyAlgoED25519 {
		return nil, errors.New("known-hosts entry is not one canonical Ed25519 key")
	}
	return key, nil
}
