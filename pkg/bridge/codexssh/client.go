package codexssh

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	clientConnectTimeout = 5 * time.Second
	clientFileLimit      = 64 << 10
)

type clientArguments struct {
	address    string
	user       string
	identity   string
	knownHosts string
}

// RunClient connects the native Codex executor's stdio to one authenticated SSH
// session. It never requests a shell, a terminal, environment, or forwarding.
// The caller retains ownership of stdin; completion never waits for its EOF.
func RunClient(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	invocation, err := parseClientArguments(args)
	if err != nil {
		return 255, err
	}
	identity, err := readClientFile(invocation.identity)
	if err != nil {
		return 255, fmt.Errorf("read SSH identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(identity)
	clear(identity)
	if err != nil {
		return 255, errors.New("parse SSH identity")
	}
	knownHosts, err := readClientFile(invocation.knownHosts)
	if err != nil {
		return 255, fmt.Errorf("read SSH known-hosts file: %w", err)
	}
	hostKey, err := clientHostKey(knownHosts, invocation.address)
	if err != nil {
		return 255, err
	}
	configuration := &ssh.ClientConfig{
		User: invocation.user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(host string, _ net.Addr, presented ssh.PublicKey) error {
			if host != invocation.address || !bytes.Equal(presented.Marshal(), hostKey.Marshal()) {
				return errors.New("strict SSH host key verification failed")
			}
			return nil
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           clientConnectTimeout,
	}
	dialer := net.Dialer{Timeout: clientConnectTimeout}
	connection, err := dialer.DialContext(ctx, "tcp", invocation.address)
	if err != nil {
		return clientFailure(ctx, "connect SSH bridge", err)
	}
	defer connection.Close()
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopCancellation()
	if err := connection.SetDeadline(time.Now().Add(clientConnectTimeout)); err != nil {
		return clientFailure(ctx, "set SSH startup deadline", err)
	}
	sshConnection, channels, requests, err := ssh.NewClientConn(connection, invocation.address, configuration)
	if err != nil {
		return clientFailure(ctx, "establish SSH connection", err)
	}
	client := ssh.NewClient(sshConnection, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return clientFailure(ctx, "open SSH session", err)
	}
	defer session.Close()
	remoteStdin, err := session.StdinPipe()
	if err != nil {
		return clientFailure(ctx, "open SSH stdin", err)
	}
	session.Stdout = stdout
	session.Stderr = stderr
	if err := session.Start("aries-codex-exec-server-v1"); err != nil {
		return clientFailure(ctx, "start Codex executor", err)
	}
	// A fast executor may have already sent its exit status and closed the
	// socket. Session.Wait still owns that buffered result.
	if err := connection.SetDeadline(time.Time{}); err != nil && !errors.Is(err, net.ErrClosed) {
		return clientFailure(ctx, "clear SSH startup deadline", err)
	}
	// Session.Stdin makes ssh.Session.Wait wait for the input copier. Native
	// executor input can remain open after remote exit or cancellation, so keep
	// that copier independent and close only the SSH input half on EOF.
	go func() {
		_, _ = io.Copy(remoteStdin, stdin)
		_ = remoteStdin.Close()
	}()
	err = session.Wait()
	if ctx.Err() != nil {
		return 255, ctx.Err()
	}
	if err == nil {
		return 0, nil
	}
	var exitError *ssh.ExitError
	if errors.As(err, &exitError) {
		if code := exitError.ExitStatus(); code >= 0 && code <= 255 {
			return code, nil
		}
	}
	return 255, fmt.Errorf("wait for Codex executor: %w", err)
}

func parseClientArguments(args []string) (clientArguments, error) {
	var invocation clientArguments
	flags := flag.NewFlagSet("aries-codex-ssh", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&invocation.address, "address", "", "SSH bridge host:port")
	flags.StringVar(&invocation.user, "user", "", "SSH bridge user")
	flags.StringVar(&invocation.identity, "identity", "", "private SSH identity file")
	flags.StringVar(&invocation.knownHosts, "known-hosts", "", "private SSH known-hosts file")
	if err := flags.Parse(args); err != nil {
		return clientArguments{}, err
	}
	if flags.NArg() != 0 {
		return clientArguments{}, errors.New("unexpected arguments after SSH client flags")
	}
	host, portText, err := net.SplitHostPort(invocation.address)
	port, portErr := strconv.Atoi(portText)
	if err != nil || host == "" || strings.ContainsAny(host, "\x00 \t\r\n") || portErr != nil || port < 1 || port > 65535 {
		return clientArguments{}, errors.New("SSH address must be host:port with a port between 1 and 65535")
	}
	if invocation.user != "aries" {
		return clientArguments{}, errors.New("SSH user must be aries")
	}
	if invocation.identity == "" || invocation.knownHosts == "" {
		return clientArguments{}, errors.New("SSH identity and known-hosts files are required")
	}
	return invocation, nil
}

func clientHostKey(content []byte, address string) (ssh.PublicKey, error) {
	marker, hosts, key, comment, rest, err := ssh.ParseKnownHosts(content)
	if err != nil || marker != "" || len(hosts) != 1 || comment != "" || len(bytes.TrimSpace(rest)) != 0 || key.Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("SSH known-hosts file must contain one unmarked Ed25519 host key")
	}
	if knownhosts.Normalize(hosts[0]) != knownhosts.Normalize(address) {
		return nil, errors.New("SSH known-hosts entry does not match the bridge address")
	}
	return key, nil
}

func readClientFile(path string) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return nil, errors.New("file path must be absolute, clean, and NUL-free")
	}
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(descriptor), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return nil, errors.New("file must be regular with owner read access and no group or world permissions")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("file must be owned by the current user")
	}
	content, err := io.ReadAll(io.LimitReader(file, clientFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(content) > clientFileLimit {
		clear(content)
		return nil, fmt.Errorf("file exceeds %d bytes", clientFileLimit)
	}
	return content, nil
}

func clientFailure(ctx context.Context, operation string, err error) (int, error) {
	if ctx.Err() != nil {
		return 255, ctx.Err()
	}
	return 255, fmt.Errorf("%s: %w", operation, err)
}
