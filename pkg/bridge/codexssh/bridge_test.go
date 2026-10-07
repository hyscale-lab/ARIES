package codexssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

type bridgeTestSandbox struct {
	mu        sync.Mutex
	commands  []core.Command
	uploads   []string
	omitProof bool
	started   chan struct{}
	block     bool
	executed  bool
}

func (s *bridgeTestSandbox) Exec(_ context.Context, c core.Command) (core.CommandResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, c)
	if s.executed {
		return core.CommandResult{}, errors.New("task-owned cleanup command ran after executor")
	}
	return core.CommandResult{}, nil
}
func (s *bridgeTestSandbox) Upload(_ context.Context, _, destination string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads = append(s.uploads, destination)
	return nil
}
func (*bridgeTestSandbox) Download(context.Context, string, string) error { return nil }
func (*bridgeTestSandbox) ContainerID() string                            { return "sandbox-id" }
func (*bridgeTestSandbox) ContainerName() string                          { return "sandbox-name" }
func (*bridgeTestSandbox) Connectivity() core.HarnessConnectivity {
	return core.HarnessConnectivity{Placement: core.RuntimePlacement{DockerNetwork: "aries-test-network"}}
}
func (*bridgeTestSandbox) RunID() string                            { return "test-run" }
func (*bridgeTestSandbox) TaskID() string                           { return "test-task" }
func (*bridgeTestSandbox) Workdir() string                          { return "/app" }
func (*bridgeTestSandbox) TaskUser(context.Context) (string, error) { return "65532:65532", nil }
func (s *bridgeTestSandbox) ExecStream(ctx context.Context, command core.Command, input io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	return core.CommandResult{}, errors.New("native executor must not use a task-owned shell wrapper")
}
func (s *bridgeTestSandbox) ExecSupervisedStream(ctx context.Context, command core.Command, input io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	if len(command.Args) == 2 && command.Args[0] == "--cleanup-stage" {
		if command.User != "0:0" || s.executed {
			return core.CommandResult{}, errors.New("unexpected standalone cleanup")
		}
		return core.CommandResult{}, nil
	}
	s.executed = true
	reader := bufio.NewReader(input)
	nonce, err := reader.ReadString('\n')
	if err != nil {
		return core.CommandResult{}, err
	}
	if len(strings.TrimSuffix(nonce, "\n")) != 64 {
		return core.CommandResult{}, errors.New("invalid nonce")
	}
	wantArgs := []string{"--codex", filepath.Dir(command.Path) + "/codex", "--stage-dir", filepath.Dir(command.Path), "--user", "65532:65532"}
	if command.User != "0:0" || !slices.Equal(command.Args, wantArgs) {
		return core.CommandResult{}, errors.New("trusted supervisor must preserve the task's native user")
	}
	if s.started != nil {
		close(s.started)
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		return core.CommandResult{}, err
	}
	if s.block {
		select {
		case <-ctx.Done():
			return core.CommandResult{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	_, _ = stdout.Write(payload)
	_, _ = io.WriteString(stderr, "executor diagnostic\n")
	if !s.omitProof {
		_, _ = io.WriteString(stderr, "\x1eARIES_CODEX_REAPED_"+strings.TrimSuffix(nonce, "\n")+"\x1f")
	}
	return core.CommandResult{}, nil
}

func bridgeFixture(t *testing.T, sandbox *bridgeTestSandbox) (*Manager, core.ToolEndpoint) {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "executable")
	if err := os.WriteFile(file, []byte("fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{ResolveListen: loopbackListen, OutputDir: dir, ClientPath: file, CodexPath: file, SupervisorPath: file, CleanupTimeout: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := m.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})
	return m, endpoint
}

func bridgeSSHClient(t *testing.T, endpoint core.ToolEndpoint) *ssh.Client {
	t.Helper()
	key, err := os.ReadFile(endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	known, err := os.ReadFile(endpoint.KnownHostsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	_, _, pinned, _, _, err := ssh.ParseKnownHosts(known)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, &ssh.ClientConfig{User: endpoint.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if !bytes.Equal(key.Marshal(), pinned.Marshal()) {
			return errors.New("wrong host")
		}
		return nil
	}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestBridgeNativeExecutorPreservesRPCAndRequiresCleanupProof(t *testing.T) {
	for _, omit := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "missing proof"}[omit], func(t *testing.T) {
			sandbox := &bridgeTestSandbox{omitProof: omit}
			manager, endpoint := bridgeFixture(t, sandbox)
			if endpoint.Workdir != "/app" || endpoint.ClientCommand != "/run/aries/ssh/aries-codex-ssh" || endpoint.KnownHostsFile != "/run/aries/ssh/known_hosts" {
				t.Fatalf("endpoint = %+v", endpoint)
			}
			client := bridgeSSHClient(t, endpoint)
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			payload := "{\"id\":1,\"method\":\"process/exec\",\"params\":{\"argv\":[\"echo\",\"a b\"]}}\n"
			session.Stdin = strings.NewReader(payload)
			var stdout, stderr bytes.Buffer
			session.Stdout = &stdout
			session.Stderr = &stderr
			err = session.Run("aries-codex-exec-server-v1")
			if !omit && err != nil {
				t.Fatal(err)
			}
			if stdout.String() != payload || strings.Contains(stderr.String(), "ARIES_CODEX_REAPED") {
				t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			_ = session.Close()
			_ = client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = manager.Stop(ctx)
			if (err != nil) != omit {
				t.Fatalf("Stop = %v, omit proof=%v", err, omit)
			}
			if !omit {
				if err := manager.Stop(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("identity survived: %v", err)
				}
				log, err := os.ReadFile(endpoint.LogPaths[0])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(log, []byte("process/exec")) || bytes.Contains(log, []byte("ARIES_CODEX_REAPED")) {
					t.Fatalf("RPC evidence missing or proof retained: %s", log)
				}
			}
		})
	}
}

func TestBridgeRejectsArbitraryCommandsAndSecondExecutor(t *testing.T) {
	_, endpoint := bridgeFixture(t, &bridgeTestSandbox{})
	client := bridgeSSHClient(t, endpoint)
	for index, command := range []string{"sh -c id", "aries-codex-exec-server-v1 extra", "aries-codex-exec-server-v1", "aries-codex-exec-server-v1"} {
		session, err := client.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		err = session.Run(command)
		_ = session.Close()
		if (err == nil) != (index == 2) {
			t.Fatalf("command %d %q: %v", index, command, err)
		}
	}
}

func TestBridgeDisconnectDrainsExecutorBeforeRevocation(t *testing.T) {
	sandbox := &bridgeTestSandbox{started: make(chan struct{}), block: true}
	manager, endpoint := bridgeFixture(t, sandbox)
	client := bridgeSSHClient(t, endpoint)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	input, err := session.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Start("aries-codex-exec-server-v1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sandbox.started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	_ = input.Close()
	_ = client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("disconnect prevented cleanup proof: %v", err)
	}
}

func loopbackListen(context.Context) (core.BridgeListen, error) {
	return core.BridgeListen{BindHost: "127.0.0.1", AdvertiseHost: "127.0.0.1"}, nil
}
