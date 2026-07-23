//go:build integration

package openclawssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const bridgeFixtureImage = "docker.io/library/busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94"

func TestBridgeExecMutatesTheEvaluatorSandbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	outputDir := t.TempDir()
	logger := logrus.New()
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
		if err := sandboxes.Stop(cleanup, live); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
	})

	bridge := newIntegrationBridge(t, outputDir, logger)
	endpoint, err := bridge.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client := dialIntegrationBridge(t, endpoint)
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
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []string{
		`"run_id":"bridge-integration"`, `"task_id":"same-state"`,
		`"operation_class":"exec"`, `"exit_code":7`, `"status":"completed"`,
		`"stdin":"streamed-input"`, `"stdin_encoding":"utf-8"`,
		`"command":"cd /aries/openclaw/`,
	} {
		if !bytes.Contains(content, []byte(evidence)) {
			t.Fatalf("tool log lacks %s: %s", evidence, content)
		}
	}
	for _, forbidden := range []string{`"stdout":`, `"stderr":`} {
		if bytes.Contains(content, []byte(forbidden)) {
			t.Fatalf("tool log retained command output field %s: %s", forbidden, content)
		}
	}
}

func TestBridgeRunsConcurrentCallsWithoutAConvoy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	outputDir := t.TempDir()
	logger := logrus.New()
	logger.SetOutput(bytes.NewBuffer(nil))
	sandboxes, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	live, err := sandboxes.Start(ctx, core.SandboxRequest{
		RunID: "bridge-concurrent", TaskID: "concurrent-calls",
		Environment: core.Environment{Image: bridgeFixtureImage, Workdir: "/work", MemoryMB: 64},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if err := sandboxes.Stop(cleanup, live); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
	})

	bridge := newIntegrationBridge(t, outputDir, logger)
	endpoint, err := bridge.Start(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if err := bridge.Stop(cleanup); err != nil {
			t.Errorf("bridge cleanup: %v", err)
		}
	})
	client := dialIntegrationBridge(t, endpoint)
	defer client.Close()

	const calls = 8
	start := make(chan struct{})
	errorsByCall := make(chan error, calls)
	var wait sync.WaitGroup
	started := time.Now()
	for index := range calls {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			session, sessionErr := client.NewSession()
			if sessionErr != nil {
				errorsByCall <- sessionErr
				return
			}
			defer session.Close()
			path := fmt.Sprintf("%s/file-%02d", openClawWorkspace, index)
			remote := encodeCanonicalTokens([]string{remoteShell, "-c", `sleep 1; if [ -e "$1" ]; then printf "1\n"; else printf "0\n"; fi`, "aries-concurrency", path})
			output, sessionErr := session.Output(remote)
			if sessionErr == nil && string(output) != "0\n" {
				sessionErr = fmt.Errorf("unexpected probe output %q", output)
			}
			errorsByCall <- sessionErr
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByCall)
	for callErr := range errorsByCall {
		if callErr != nil {
			t.Fatal(callErr)
		}
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("%d concurrent calls formed a %s convoy", calls, elapsed)
	}

	const activeCalls = 4
	activeSessions := make([]*ssh.Session, 0, activeCalls)
	for index := range activeCalls {
		session, sessionErr := client.NewSession()
		if sessionErr != nil {
			t.Fatal(sessionErr)
		}
		activeSessions = append(activeSessions, session)
		command := fmt.Sprintf("printf '%%s ' \"$$\" > /work/active-%d.pid; awk '{print $22}' /proc/$$/stat >> /work/active-%d.pid; sleep 60", index, index)
		if sessionErr := session.Start(encodeCanonicalTokens([]string{remoteShell, "-c", command})); sessionErr != nil {
			t.Fatal(sessionErr)
		}
	}
	sandbox := live.(*dockersandbox.Sandbox)
	ready, err := sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh", Args: []string{"-c", "attempt=0; until test -f /work/active-0.pid && test -f /work/active-1.pid && test -f /work/active-2.pid && test -f /work/active-3.pid; do attempt=$((attempt+1)); [ \"$attempt\" -lt 100 ] || exit 1; sleep 0.02; done"},
	})
	if err != nil || ready.ExitCode != 0 {
		t.Fatalf("concurrent calls did not all start: %#v, %v", ready, err)
	}
	stopping := time.Now()
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(stopping); elapsed > 8*time.Second {
		t.Fatalf("bridge revocation waited %s for concurrent calls", elapsed)
	}
	for _, session := range activeSessions {
		_ = session.Close()
	}
	postRevocation, err := sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh", Args: []string{"-c", "failed=0; for file in /work/active-*.pid; do read -r pid started < \"$file\"; if test -r \"/proc/$pid/stat\" && test \"$(awk '{print $22}' \"/proc/$pid/stat\")\" = \"$started\"; then printf 'live %s pid=%s command=' \"$file\" \"$pid\"; tr '\\0' ' ' < \"/proc/$pid/cmdline\"; printf '\\n'; failed=1; fi; done; test \"$failed\" -eq 0 || exit 1; printf evaluator > /work/after-bridge"},
	})
	if err != nil || postRevocation.ExitCode != 0 {
		t.Fatalf("sandbox was not usable for evaluation after revocation: %#v, %v", postRevocation, err)
	}
}

func newIntegrationBridge(t *testing.T, outputDir string, logger *logrus.Logger) *Manager {
	t.Helper()
	clientHelper := filepath.Join(t.TempDir(), "aries-ssh")
	if err := os.WriteFile(clientHelper, []byte("integration fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	bridge, err := New(Options{OutputDir: outputDir, ClientPath: clientHelper, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	return bridge
}

func dialIntegrationBridge(t *testing.T, endpoint core.ToolEndpoint) *ssh.Client {
	t.Helper()
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
	return client
}
