package client

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const (
	lockedHostName = "127.0.0.1"
	lockedPort     = 2222
)

func TestClientArgumentsRequireExactPinnedOrder(t *testing.T) {
	directory := testOpenClawSSHDirectory(t)
	configPath := filepath.Join(directory, "config")
	remote := "arbitrary remote command"
	want := []string{"-F", configPath, "-T", "-o", "RequestTTY=no", lockedHostAlias, remote}
	if _, err := parseClientArguments(want); err != nil {
		t.Fatalf("exact arguments rejected: %v", err)
	}
	mutations := [][]string{
		want[:6],
		{"-T", "-F", configPath, "-o", "RequestTTY=no", lockedHostAlias, remote},
		{"-F", configPath, "-tt", "-o", "RequestTTY=force", lockedHostAlias, remote},
		{"-F", configPath, "-T", "-o", "ProxyCommand=bad", lockedHostAlias, remote},
		{"-F", configPath, "-T", "-o", "RequestTTY=no", lockedHostAlias, "extra", remote},
		{"-F", configPath, "-T", "-o", "RequestTTY=no", "other-host", remote},
	}
	for _, mutation := range mutations {
		if _, err := parseClientArguments(mutation); err == nil {
			t.Errorf("arguments %#v unexpectedly accepted", mutation)
		}
	}
}

func TestLoadClientConfigRequiresExactContentAndPrivatePaths(t *testing.T) {
	directory := testOpenClawSSHDirectory(t)
	path := filepath.Join(directory, "config")
	content := lockedConfigContent()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := loadClientConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.hostName != lockedHostName || configuration.port != lockedPort {
		t.Fatalf("configuration = %#v", configuration)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientConfig(path); err == nil {
		t.Fatal("mode-0644 config was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(content, "  BatchMode yes\n", "  ForwardAgent yes\n  BatchMode yes\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientConfig(path); err == nil {
		t.Fatal("unknown config directive was accepted")
	}

	outside := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(outside, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientConfig(outside); err == nil {
		t.Fatal("config outside OpenClaw temporary path was accepted")
	}
}

func TestLoadClientConfigAcceptsDynamicGatewayAndPort(t *testing.T) {
	directory := testOpenClawSSHDirectory(t)
	path := filepath.Join(directory, "config")
	content := strings.ReplaceAll(lockedConfigContent(), "HostName "+lockedHostName, "HostName 172.23.0.1")
	content = strings.ReplaceAll(content, "Port "+strconv.Itoa(lockedPort), "Port 49152")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := loadClientConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.hostName != "172.23.0.1" || configuration.port != 49152 {
		t.Fatalf("configuration = %#v", configuration)
	}
}

func TestLoadClientConfigAcceptsOpenClawUserTemporaryRoot(t *testing.T) {
	root := filepath.Join(os.TempDir(), "openclaw-"+strconv.Itoa(os.Geteuid()))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(root, "openclaw-sandbox-ssh-unit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(directory)
		_ = os.Remove(root)
	})
	path := filepath.Join(directory, "config")
	if err := os.WriteFile(path, []byte(lockedConfigContent()), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadClientConfig(path); err != nil {
		t.Fatalf("OpenClaw user temporary root rejected: %v", err)
	}
}

func lockedConfigContent() string {
	return strings.Join([]string{
		"Host " + lockedHostAlias,
		"  HostName " + lockedHostName,
		"  Port 2222",
		"  BatchMode yes",
		"  ConnectTimeout 5",
		"  ServerAliveInterval 15",
		"  ServerAliveCountMax 3",
		"  StrictHostKeyChecking no",
		"  UpdateHostKeys no",
		"  User " + lockedUsername,
		"  UserKnownHostsFile /dev/null",
	}, "\n") + "\n"
}

func testOpenClawSSHDirectory(t *testing.T) string {
	t.Helper()
	root := filepath.Join(os.TempDir(), "openclaw")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(root, "openclaw-sandbox-ssh-unit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(directory)
		_ = os.Remove(root)
	})
	return directory
}

func TestClientArgumentsDoNotInterpretRemoteCommand(t *testing.T) {
	directory := testOpenClawSSHDirectory(t)
	for _, remote := range []string{"true", "echo \"quotes\"; $(false)\nλ", "'unterminated", ""} {
		args := []string{"-F", filepath.Join(directory, "config"), "-T", "-o", "RequestTTY=no", lockedHostAlias, remote}
		got, err := parseClientArguments(args)
		if err != nil || got.remote != remote {
			t.Fatalf("remote=%q got=%q err=%v", remote, got.remote, err)
		}
	}
}

func TestLoadClientConfigAcceptsDNSAndRejectsHostInjection(t *testing.T) {
	directory := testOpenClawSSHDirectory(t)
	path := filepath.Join(directory, "config")
	for _, host := range []string{"bridge.internal", "host with spaces", "$(bad)", "bridge\nProxyCommand bad"} {
		content := strings.Replace(lockedConfigContent(), "HostName "+lockedHostName, "HostName "+host, 1)
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadClientConfig(path)
		if (err == nil) != (host == "bridge.internal") {
			t.Fatalf("host=%q err=%v", host, err)
		}
	}
}

func TestSecureRegularFilesRejectSymlinkAndOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureRegularFile(path, 0600, 4); err == nil {
		t.Fatal("oversize accepted")
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureRegularFile(link, 0600, 64); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestSecureRegularFileRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecureRegularFile(path, 0600, 64); err == nil {
		t.Fatal("FIFO accepted")
	}
}
