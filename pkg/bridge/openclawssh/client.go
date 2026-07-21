package openclawssh

import (
	"bytes"
	"context"
	"errors"
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
)

const (
	clientContainerPath     = "/opt/aries/bin/aries-ssh"
	serverContainerPath     = "/opt/aries/bin/aries-ssh-server"
	identityContainerPath   = "/run/aries/ssh/id_ed25519"
	knownHostsContainerPath = "/run/aries/ssh/known_hosts"
	lockedHostAlias         = "openclaw-sandbox"
	lockedHostName          = "task-sandbox"
	lockedUsername          = "aries"
	lockedPort              = 2222
	lockedConnectTimeout    = 5 * time.Second
	lockedKeepalive         = 15 * time.Second
	maxClientFile           = 64 << 10
)

type clientInvocation struct {
	configPath string
	remote     string
}

type clientConfig struct {
	hostName       string
	port           int
	user           string
	identityFile   string
	knownHostsFile string
}

// ClientMain implements only the exact non-TTY OpenSSH argv used by the
// pinned OpenClaw release. It returns a process exit code and writes no secret
// material.
func ClientMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	invocation, err := parseClientArguments(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh: %v\n", err)
		return 255
	}
	configuration, err := loadClientConfig(invocation.configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh: %v\n", err)
		return 255
	}
	code, err := runSSHClient(context.Background(), configuration, invocation.remote, stdin, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh: %v\n", err)
		return 255
	}
	return code
}

func parseClientArguments(args []string) (clientInvocation, error) {
	if len(args) != 7 {
		return clientInvocation{}, errors.New("expected exactly -F CONFIG -T -o RequestTTY=no openclaw-sandbox REMOTE_COMMAND")
	}
	if args[0] != "-F" || args[2] != "-T" || args[3] != "-o" || args[4] != "RequestTTY=no" || args[5] != lockedHostAlias {
		return clientInvocation{}, errors.New("arguments do not match the locked OpenClaw non-TTY order")
	}
	if _, err := validateConfigPath(args[1]); err != nil {
		return clientInvocation{}, err
	}
	if strings.ContainsRune(args[6], 0) {
		return clientInvocation{}, errors.New("remote command contains NUL")
	}
	if _, err := decodeRemoteCommand(args[6]); err != nil {
		return clientInvocation{}, fmt.Errorf("reject noncanonical remote command: %w", err)
	}
	return clientInvocation{configPath: args[1], remote: args[6]}, nil
}

func validateConfigPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "config" {
		return "", errors.New("SSH config path must be an absolute clean .../openclaw-ssh-*/config path")
	}
	directory := filepath.Dir(path)
	if filepath.Dir(directory) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(directory), "openclaw-ssh-") || filepath.Base(directory) == "openclaw-ssh-" {
		return "", errors.New("SSH config parent does not match OpenClaw's private temporary directory")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", fmt.Errorf("inspect SSH config directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return "", errors.New("SSH config directory must be a private mode-0700 directory")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return "", errors.New("SSH config directory must be owned by the current user")
	}
	return path, nil
}

func loadClientConfig(path string) (clientConfig, error) {
	if _, err := validateConfigPath(path); err != nil {
		return clientConfig{}, err
	}
	content, err := readSecureRegularFile(path, 0o600, maxClientFile)
	if err != nil {
		return clientConfig{}, fmt.Errorf("read SSH config: %w", err)
	}
	want := []string{
		"Host " + lockedHostAlias,
		"  HostName " + lockedHostName,
		"  Port " + strconv.Itoa(lockedPort),
		"  BatchMode yes",
		"  ConnectTimeout 5",
		"  ServerAliveInterval 15",
		"  ServerAliveCountMax 3",
		"  StrictHostKeyChecking yes",
		"  UpdateHostKeys no",
		"  User " + lockedUsername,
		"  UserKnownHostsFile " + knownHostsContainerPath,
		"  IdentityFile " + identityContainerPath,
		"  IdentitiesOnly yes",
	}
	if string(content) != strings.Join(want, "\n")+"\n" {
		return clientConfig{}, errors.New("SSH config does not exactly match the pinned OpenClaw directive order and values")
	}
	return clientConfig{
		hostName:       lockedHostName,
		port:           lockedPort,
		user:           lockedUsername,
		identityFile:   identityContainerPath,
		knownHostsFile: knownHostsContainerPath,
	}, nil
}

func runSSHClient(ctx context.Context, configuration clientConfig, remote string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	identity, err := readSecureRegularFile(configuration.identityFile, 0o600, maxClientFile)
	if err != nil {
		return 255, fmt.Errorf("read SSH identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(identity)
	if err != nil {
		return 255, errors.New("parse SSH identity")
	}
	knownHosts, err := readSecureRegularFile(configuration.knownHostsFile, 0o600, maxClientFile)
	if err != nil {
		return 255, fmt.Errorf("read SSH known-hosts file: %w", err)
	}
	hostKey, err := parseLockedKnownHost(knownHosts)
	if err != nil {
		return 255, err
	}
	address := net.JoinHostPort(configuration.hostName, strconv.Itoa(configuration.port))
	dialer := net.Dialer{Timeout: lockedConnectTimeout}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return 255, fmt.Errorf("connect %s: %w", address, err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(lockedConnectTimeout))
	clientConfiguration := &ssh.ClientConfig{
		User: configuration.user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(host string, remoteAddress net.Addr, presented ssh.PublicKey) error {
			if host != address || !bytes.Equal(presented.Marshal(), hostKey.Marshal()) {
				return errors.New("strict SSH host-key verification failed")
			}
			return nil
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           lockedConnectTimeout,
	}
	sshConnection, channels, requests, err := ssh.NewClientConn(connection, address, clientConfiguration)
	if err != nil {
		return 255, fmt.Errorf("establish SSH connection: %w", err)
	}
	_ = connection.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConnection, channels, requests)
	defer client.Close()
	stopKeepalive := startKeepalive(client)
	defer stopKeepalive()
	session, err := client.NewSession()
	if err != nil {
		return 255, fmt.Errorf("open SSH session: %w", err)
	}
	defer session.Close()
	session.Stdin = stdin
	session.Stdout = stdout
	session.Stderr = stderr
	err = session.Run(remote)
	if err == nil {
		return 0, nil
	}
	var exitError *ssh.ExitError
	if errors.As(err, &exitError) {
		status := exitError.ExitStatus()
		if status >= 0 && status <= 255 {
			return status, nil
		}
	}
	return 255, fmt.Errorf("run SSH command: %w", err)
}

func startKeepalive(client *ssh.Client) func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(lockedKeepalive)
		defer ticker.Stop()
		failures := 0
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					failures++
					if failures >= 3 {
						_ = client.Close()
						return
					}
				} else {
					failures = 0
				}
			}
		}
	}()
	return func() { close(done) }
}

func parseLockedKnownHost(content []byte) (ssh.PublicKey, error) {
	line := strings.TrimSuffix(string(content), "\n")
	if line == string(content) || strings.Contains(line, "\n") {
		return nil, errors.New("known-hosts file must contain exactly one newline-terminated entry")
	}
	prefix := "[" + lockedHostName + "]:" + strconv.Itoa(lockedPort) + " "
	if !strings.HasPrefix(line, prefix) {
		return nil, errors.New("known-hosts entry does not match the locked host and port")
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(strings.TrimPrefix(line, prefix) + "\n"))
	if err != nil || len(rest) != 0 || len(options) != 0 || comment != "" || key.Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("known-hosts entry is not one canonical Ed25519 key")
	}
	return key, nil
}

func readSecureRegularFile(path string, mode os.FileMode, limit int64) ([]byte, error) {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("path must be absolute, clean, and NUL-free")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		return nil, fmt.Errorf("file must be regular mode %04o", mode)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("file must be owned by the current user")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return content, nil
}
