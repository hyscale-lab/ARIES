//go:build integration

package openclawssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const bridgeFixtureImage = "docker.io/library/busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94"

func TestBridgeExecMutatesTheEvaluatorSandbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	outputDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sandboxes, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	live, err := sandboxes.Start(ctx, core.SandboxRequest{
		RunID: "bridge-integration", TaskID: "same-state",
		Environment: core.Environment{Image: bridgeFixtureImage, Workdir: "/work", MemoryMB: 64},
	})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*dockersandbox.Sandbox)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if err := sandbox.Stop(cleanup); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
	})

	clientHelper := filepath.Join(t.TempDir(), "aries-ssh")
	if err := os.WriteFile(clientHelper, []byte("integration fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	bridge, err := New(Options{OutputDir: outputDir, ClientPath: clientHelper, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := bridge.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.ReadFile(endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := knownhosts.New(endpoint.KnownHostsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, &ssh.ClientConfig{
		User: endpoint.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKey, HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	session.Stdin = strings.NewReader("streamed-input")
	session.Stdout, session.Stderr = &stdout, &stderr
	remote := encodeCanonicalTokens([]string{remoteShell, "-c", "cd " + openClawWorkspace + "; cat > bridge-state; cat bridge-state; printf tool-stderr >&2; exit 7"})
	err = session.Run(remote)
	_ = client.Close()
	var exitError *ssh.ExitError
	if !errors.As(err, &exitError) || exitError.ExitStatus() != 7 || stdout.String() != "streamed-input" || stderr.String() != "tool-stderr" {
		t.Fatalf("SSH exec = err %v stdout %q stderr %q", err, stdout.String(), stderr.String())
	}
	direct, err := sandbox.Exec(ctx, core.Command{Path: "/bin/cat", Args: []string{"/work/bridge-state"}})
	if err != nil || direct.ExitCode != 0 || direct.Stdout != "streamed-input" {
		t.Fatalf("evaluator read = %#v, %v", direct, err)
	}
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity remains after Stop: %v", err)
	}
	if connection, err := net.DialTimeout("tcp", endpoint.Address, 200*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("bridge listener still accepts connections after Stop")
	}
	info, err := os.Stat(endpoint.LogPaths[0])
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("retained tool log = %v, %v", info, err)
	}
	content, err := os.ReadFile(endpoint.LogPaths[0])
	for _, evidence := range []string{
		`"run_id":"bridge-integration"`, `"task_id":"same-state"`,
		`"operation_class":"exec"`, `"exit_code":7`, `"status":"completed"`,
	} {
		if !bytes.Contains(content, []byte(evidence)) {
			t.Fatalf("tool log lacks %s: %s", evidence, content)
		}
	}
	if err != nil || bytes.Contains(content, []byte("streamed-input")) || bytes.Contains(content, []byte("tool-stderr")) {
		t.Fatalf("tool log is missing or unredacted: %s, %v", content, err)
	}
}
