package openclawssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

const (
	lockedHostName = "127.0.0.1"
	lockedPort     = 2222
)

func TestClientArgumentsRequireExactPinnedOrder(t *testing.T) {
	directory := testOpenClawSSHDirectory(t)
	configPath := filepath.Join(directory, "config")
	remote := encodeCanonicalTokens([]string{remoteShell, "-c", "true"})
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
		{"-F", configPath, "-T", "-o", "RequestTTY=no", lockedHostAlias, "true"},
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

func TestKnownHostsRequiresExactHostAndEd25519Key(t *testing.T) {
	t.Parallel()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("[" + lockedHostName + "]:" + "2222 " + string(ssh.MarshalAuthorizedKey(key)))
	if _, err := parseLockedKnownHost(content, lockedHostName, lockedPort); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{
		[]byte("task-sandbox ssh-ed25519 bad\n"),
		append(content, '\n'),
		[]byte(strings.Replace(string(content), "2222", "22", 1)),
		[]byte(strings.TrimSuffix(string(content), "\n") + " comment\n"),
	} {
		if _, err := parseLockedKnownHost(invalid, lockedHostName, lockedPort); err == nil {
			t.Errorf("known-hosts bytes %q unexpectedly accepted", invalid)
		}
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
		"  StrictHostKeyChecking yes",
		"  UpdateHostKeys no",
		"  User " + lockedUsername,
		"  UserKnownHostsFile " + knownHostsContainerPath,
		"  IdentityFile " + identityContainerPath,
		"  IdentitiesOnly yes",
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
