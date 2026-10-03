package openclawssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Split in two, the bridge and the runner must still produce exactly the
// endpoint OpenClaw gets in-process: the helper, the identity and a
// known_hosts that verifies the host key the bridge actually serves.
func TestSplitBridgeServesTheRunnersKeyAndTheRunnersKnownHostsVerifies(t *testing.T) {
	clientPath := filepath.Join(resolvedTempDir(t), "aries-ssh")
	if err := os.WriteFile(clientPath, []byte("test client helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	runnerDir := filepath.Join(resolvedTempDir(t), "contract-task", "bridge")
	credentials, err := NewCredentials(runnerDir, clientPath)
	if err != nil {
		t.Fatal(err)
	}
	requireFileMode(t, filepath.Join(runnerDir, "aries-ssh"), 0o555)
	requireFileMode(t, filepath.Join(runnerDir, "id_ed25519"), 0o600)

	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	bridgeDir := resolvedTempDir(t)
	manager, err := New(Options{
		OutputDir: bridgeDir, CleanupTimeout: time.Second, AdvertiseHost: "127.0.0.1",
		Keys: &SessionKeys{Host: host, Authorized: credentials.AuthorizedKey()},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	served, err := manager.Start(ctx, &contractSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(ctx)
	if served.ClientSourceFile != "" || served.IdentitySourceFile != "" || served.KnownHostsSourceFile != "" {
		t.Fatalf("with external keys the bridge must name no source files: %+v", served)
	}
	for _, name := range []string{"aries-ssh", "id_ed25519"} {
		if _, err := os.Stat(filepath.Join(bridgeDir, "contract-task", "bridge", name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("bridge side staged %s: %v", name, err)
		}
	}

	endpoint, err := credentials.Endpoint(served.Address, host.PublicKey(), served.Network)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.ClientCommand != clientContainerPath || endpoint.IdentityFile != identityContainerPath || endpoint.KnownHostsFile != knownHostsContainerPath {
		t.Fatalf("container paths differ from the in-process bridge: %+v", endpoint)
	}
	requireFileMode(t, endpoint.KnownHostsSourceFile, 0o600)
	// bridgeClientConfig verifies the host key against the runner-written
	// known_hosts, which is what aries-ssh does inside the agent.
	client, err := ssh.Dial("tcp", endpoint.Address, bridgeClientConfig(t, endpoint))
	if err != nil {
		t.Fatalf("the runner's key and known_hosts do not reach the bridge: %v", err)
	}
	_ = client.Close()

	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := credentials.Revoke(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"aries-ssh", "id_ed25519", "known_hosts"} {
		if _, err := os.Stat(filepath.Join(runnerDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Revoke left %s: %v", name, err)
		}
	}
}

func TestExternalKeysMustBeComplete(t *testing.T) {
	_, hostPrivate, _ := ed25519.GenerateKey(rand.Reader)
	host, _ := ssh.NewSignerFromKey(hostPrivate)
	if _, err := New(Options{OutputDir: resolvedTempDir(t), Keys: &SessionKeys{Host: host}}); err == nil {
		t.Error("accepted session keys without an authorized key")
	}
}
