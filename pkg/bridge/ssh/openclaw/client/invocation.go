// Package client implements the SSH client invocation emitted by OpenClaw.
// Remote commands are forwarded unchanged for the server dialect to interpret.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	lockedHostAlias      = "openclaw-sandbox"
	lockedUsername       = "aries"
	lockedConnectTimeout = 5 * time.Second
	lockedKeepalive      = 15 * time.Second
	maxClientFile        = 64 << 10
)

type clientInvocation struct {
	configPath string
	remote     string
}

type clientConfig struct {
	hostName string
	port     int
}

// Main implements only the exact non-TTY OpenSSH argv used by the
// pinned OpenClaw release. It returns a process exit code and writes no secret
// material.
func Main(ctx context.Context, args []string, stdin io.ReadCloser, stdout, stderr io.Writer) int {
	invocation, err := parseClientArguments(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh-client: %v\n", err)
		return 255
	}
	configuration, err := loadClientConfig(invocation.configPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh-client: %v\n", err)
		return 255
	}
	code, err := runSSHClient(ctx, configuration, invocation.remote, stdin, stdout, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh-client: %v\n", err)
		return 255
	}
	return code
}

// parseClientArguments recognizes the supported client CLI, not the remote
// command grammar. The server dialect alone decides whether remote may execute;
// loadClientConfig owns filesystem and configuration validation.
func parseClientArguments(args []string) (clientInvocation, error) {
	if len(args) != 7 {
		return clientInvocation{}, errors.New("expected exactly -F CONFIG -T -o RequestTTY=no openclaw-sandbox REMOTE_COMMAND")
	}
	if args[0] != "-F" || args[2] != "-T" || args[3] != "-o" || args[4] != "RequestTTY=no" || args[5] != lockedHostAlias {
		return clientInvocation{}, errors.New("arguments do not match the locked OpenClaw non-TTY order")
	}
	if strings.ContainsRune(args[6], 0) {
		return clientInvocation{}, errors.New("remote command contains NUL")
	}
	return clientInvocation{configPath: args[1], remote: args[6]}, nil
}

func validateConfigPath(path string) error {
	if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Base(path) != "config" {
		return errors.New("SSH config path must be an absolute clean .../openclaw-sandbox-ssh-*/config path")
	}
	directory := filepath.Dir(path)
	root := filepath.Dir(directory)
	preferredRoot := filepath.Join(filepath.Clean(os.TempDir()), "openclaw")
	fallbackRoot := filepath.Join(filepath.Clean(os.TempDir()), "openclaw-"+strconv.Itoa(os.Geteuid()))
	if root != preferredRoot && root != fallbackRoot || !strings.HasPrefix(filepath.Base(directory), "openclaw-sandbox-ssh-") || filepath.Base(directory) == "openclaw-sandbox-ssh-" {
		return errors.New("SSH config parent does not match OpenClaw's private temporary directory")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect OpenClaw SSH temporary root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode().Perm() != 0o700 {
		return errors.New("OpenClaw SSH temporary root must be a private mode-0700 directory")
	}
	if stat, ok := rootInfo.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("OpenClaw SSH temporary root must be owned by the current user")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect SSH config directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("SSH config directory must be a private mode-0700 directory")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("SSH config directory must be owned by the current user")
	}
	return nil
}

func loadClientConfig(path string) (clientConfig, error) {
	if err := validateConfigPath(path); err != nil {
		return clientConfig{}, err
	}
	content, err := readSecureRegularFile(path, 0o600, maxClientFile)
	if err != nil {
		return clientConfig{}, fmt.Errorf("read SSH config: %w", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if !strings.HasSuffix(string(content), "\n") || len(lines) != 11 {
		return clientConfig{}, errors.New("SSH config does not exactly match the pinned directive count")
	}
	const hostPrefix = "  HostName "
	const portPrefix = "  Port "
	if !strings.HasPrefix(lines[1], hostPrefix) || !strings.HasPrefix(lines[2], portPrefix) {
		return clientConfig{}, errors.New("SSH config does not contain HostName and Port in the pinned order")
	}
	hostName := strings.TrimPrefix(lines[1], hostPrefix)
	if !core.ValidEndpointHost(hostName) {
		return clientConfig{}, errors.New("SSH HostName must be one valid endpoint host")
	}
	port, err := strconv.Atoi(strings.TrimPrefix(lines[2], portPrefix))
	if err != nil || port < 1 || port > 65535 {
		return clientConfig{}, errors.New("SSH Port must be between 1 and 65535")
	}
	want := []string{
		"Host " + lockedHostAlias,
		"  HostName " + hostName,
		"  Port " + strconv.Itoa(port),
		"  BatchMode yes",
		"  ConnectTimeout 5",
		"  ServerAliveInterval 15",
		"  ServerAliveCountMax 3",
		"  StrictHostKeyChecking no",
		"  UpdateHostKeys no",
		"  User " + lockedUsername,
		"  UserKnownHostsFile /dev/null",
	}
	if string(content) != strings.Join(want, "\n")+"\n" {
		return clientConfig{}, errors.New("SSH config does not exactly match the pinned OpenClaw directive order and values")
	}
	return clientConfig{hostName: hostName, port: port}, nil
}
