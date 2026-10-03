package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"golang.org/x/crypto/ssh"
)

const (
	testNamespace = "aries"
	testSandboxID = "0123456789abcdef"
	testPod       = "aries-task-" + testSandboxID
)

// runnerSandbox is what the runner holds: enough to name the pod, no ability
// to exec. The bridge pod must never be handed it.
type runnerSandbox struct{ namespace string }

func (runnerSandbox) Exec(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, errors.New("the runner-side sandbox must not be exec'd by the bridge")
}
func (runnerSandbox) Upload(context.Context, string, string) error   { return nil }
func (runnerSandbox) Download(context.Context, string, string) error { return nil }
func (s runnerSandbox) Namespace() string                            { return s.namespace }
func (runnerSandbox) ContainerName() string                          { return testPod }
func (runnerSandbox) SandboxID() string                              { return testSandboxID }
func (runnerSandbox) Workdir() string                                { return "/app" }
func (runnerSandbox) RunID() string                                  { return "run-1" }
func (runnerSandbox) TaskID() string                                 { return "task-1" }

// podSandbox is what the bridge pod attaches to: it records commands and
// answers them, as the Kubernetes sandbox would through kubectl exec.
type podSandbox struct {
	mu       sync.Mutex
	commands []string
}

func (s *podSandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	return s.ExecStream(ctx, command, nil, io.Discard, io.Discard)
}
func (s *podSandbox) ExecStream(_ context.Context, command core.Command, stdin io.Reader, stdout, _ io.Writer) (core.CommandResult, error) {
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	s.mu.Lock()
	s.commands = append(s.commands, strings.Join(append([]string{command.Path}, command.Args...), " "))
	s.mu.Unlock()
	_, _ = io.WriteString(stdout, "from-sandbox\n")
	return core.CommandResult{ExitCode: 0}, nil
}
func (*podSandbox) Upload(context.Context, string, string) error   { return nil }
func (*podSandbox) Download(context.Context, string, string) error { return nil }
func (*podSandbox) ContainerID() string                            { return testNamespace + "/" + testPod }
func (*podSandbox) ContainerName() string                          { return testPod }
func (*podSandbox) NetworkName() string                            { return testNamespace + "/" + testPod }
func (*podSandbox) NetworkGateway(context.Context) (string, error) {
	return "", errors.New("advertised mode must not ask for a gateway")
}
func (*podSandbox) RunID() string   { return "run-1" }
func (*podSandbox) TaskID() string  { return "task-1" }
func (*podSandbox) Workdir() string { return "/app" }

// fakeCluster is kubectl for tests: `exec` into the bridge pod runs the real
// ctl and collect code against a real daemon socket, and the pod can be made
// to vanish or stop answering.
type fakeCluster struct {
	socket string

	mu       sync.Mutex
	podName  string
	podUID   string
	gone     bool
	mute     bool
	execLog  []string
	requests []Request
}

func (f *fakeCluster) Run(_ context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	f.mu.Lock()
	podName, podUID, gone, mute := f.podName, f.podUID, f.gone, f.mute
	f.execLog = append(f.execLog, strings.Join(args, " "))
	f.mu.Unlock()
	if stdout == nil {
		stdout = io.Discard
	}
	switch {
	case len(args) >= 2 && args[0] == "get" && args[1] == "pods":
		items := []any{}
		if !gone {
			items = append(items, map[string]any{
				"metadata": map[string]any{"name": podName, "uid": podUID},
				"status": map[string]any{"phase": "Running", "containerStatuses": []any{
					map[string]any{"name": ContainerName, "ready": true},
				}},
			})
		}
		return json.NewEncoder(stdout).Encode(map[string]any{"items": items})
	case len(args) >= 2 && args[0] == "get" && args[1] == "pod":
		if !gone {
			_, _ = io.WriteString(stdout, podUID)
		}
		return nil
	case len(args) > 0 && args[0] == "exec":
		if gone {
			return errors.New(`pods "` + podName + `" not found`)
		}
		if mute {
			return errors.New("error dialing backend: connection refused")
		}
		command := args[len(args)-1]
		if command == "ctl" {
			content, _ := io.ReadAll(stdin)
			var request Request
			_ = json.Unmarshal(content, &request)
			f.mu.Lock()
			f.requests = append(f.requests, request)
			f.mu.Unlock()
			return RunCtl(f.socket, bytes.NewReader(content), stdout, 10*time.Second)
		}
		return RunCollect(f.socket, command, stdout, 10*time.Second)
	}
	return fmt.Errorf("unexpected kubectl %v", args)
}

func (f *fakeCluster) set(update func(*fakeCluster)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	update(f)
}

type harness struct {
	daemon  *Daemon
	cluster *fakeCluster
	sandbox *podSandbox
	output  string
	stop    func()
}

// startDaemon runs a real daemon serving real Hermes SSH bridges on loopback,
// the same wiring cmd/aries-bridge uses with the pod IP.
func startDaemon(t *testing.T, attach AttachFunc) *harness {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes; $TMPDIR on macOS is long.
	socketDir, err := os.MkdirTemp("/tmp", "arb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	sandbox := &podSandbox{}
	if attach == nil {
		attach = func(_ context.Context, request GrantRequest) (runner.Sandbox, error) {
			if sandbox := request.Sandbox; sandbox.PodName != testPod || sandbox.SandboxID != testSandboxID || sandbox.Namespace != testNamespace {
				return nil, errors.New("attach got the wrong pod")
			}
			return sandbox, nil
		}
	}
	daemon, err := NewDaemon(DaemonOptions{
		OutputDir: realTempDir(t), Backend: BackendKubernetes, Namespace: testNamespace, Attach: attach,
		NewBridge: func(request GrantRequest, outputDir string, host ssh.Signer, authorized ssh.PublicKey) (runner.ToolBridge, error) {
			return hermesssh.New(hermesssh.Options{
				OutputDir: outputDir, AdvertiseHost: "127.0.0.1", CleanupTimeout: 5 * time.Second,
				OmitRawLog: !request.RetainRawLog, Keys: &hermesssh.SessionKeys{Host: host, Authorized: authorized},
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(socketDir, "c.sock")
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- daemon.Serve(ctx, socket) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h := &harness{
		daemon: daemon, sandbox: sandbox, output: realTempDir(t),
		cluster: &fakeCluster{socket: socket, podName: "aries-bridge-7d9f", podUID: "uid-1"},
	}
	h.stop = func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := daemon.Close(closeCtx); err != nil {
			t.Errorf("close: %v", err)
		}
	}
	t.Cleanup(h.stop)
	return h
}

// realTempDir resolves symbolic links in the temporary directory, since the
// bridges refuse a private directory reached through one (macOS's $TMPDIR is).
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func (h *harness) client(t *testing.T) *Client {
	t.Helper()
	client, err := New(Options{
		BridgeType: "hermes-ssh", Transport: &KubeTransport{Namespace: testNamespace, Kubectl: h.cluster}, OutputDir: h.output,
		NewCredentials: func(dir string) (Credentials, error) { return hermesssh.NewCredentials(dir) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func dial(t *testing.T, endpoint core.ToolEndpoint, identity []byte) (*ssh.Client, error) {
	t.Helper()
	signer, err := ssh.ParsePrivateKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.Dial("tcp", endpoint.Address, &ssh.ClientConfig{
		User: endpoint.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second,
	})
}

// The whole path: the runner grants over kubectl exec, the harness reaches
// the bridge pod with the runner's key, a tool call lands in the sandbox, and
// Stop revokes, removes the key and brings the tool-call log home.
func TestGrantServesToolCallsAndStopBringsTheEvidenceBack(t *testing.T) {
	h := startDaemon(t, nil)
	ctx := context.Background()
	options := Options{BridgeType: "hermes-ssh", Transport: &KubeTransport{Namespace: testNamespace, Kubectl: h.cluster}, OutputDir: h.output,
		NewCredentials: func(dir string) (Credentials, error) { return hermesssh.NewCredentials(dir) }}
	if err := Preflight(ctx, options); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	client := h.client(t)
	endpoint, err := client.Start(ctx, runnerSandbox{namespace: testNamespace})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	artifacts := filepath.Join(h.output, "task-1", "bridge")
	if endpoint.IdentitySourceFile != filepath.Join(artifacts, "id_ed25519") {
		t.Fatalf("identity source = %q, want it on the runner under %s", endpoint.IdentitySourceFile, artifacts)
	}
	if info, err := os.Stat(endpoint.IdentitySourceFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("identity must exist 0600 on the runner: %v %v", info, err)
	}
	if !strings.HasPrefix(endpoint.Address, "127.0.0.1:") || endpoint.Network != testNamespace+"/"+testPod {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	if len(endpoint.LogPaths) != 1 || endpoint.LogPaths[0] != filepath.Join(artifacts, "tool-calls.jsonl") {
		t.Fatalf("log paths = %v, want the runner-local tool-calls.jsonl", endpoint.LogPaths)
	}
	known, err := os.ReadFile(filepath.Join(artifacts, "known_hosts"))
	if err != nil || !strings.HasPrefix(string(known), "[127.0.0.1]:") {
		t.Fatalf("known_hosts = %q %v", known, err)
	}

	// Kept in memory, so the redial after Stop tests the bridge refusing the
	// key rather than the file being gone.
	identity, err := os.ReadFile(endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatal(err)
	}
	client1, err := dial(t, endpoint, identity)
	if err != nil {
		t.Fatalf("harness could not reach the bridge with the runner's key: %v", err)
	}
	session, err := client1.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := session.Output("bash -c 'echo hi'")
	session.Close()
	client1.Close()
	if err != nil || string(out) != "from-sandbox\n" {
		t.Fatalf("tool call = %q %v", out, err)
	}

	if err := client.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("private identity survived revocation: %v", err)
	}
	log, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil || !strings.Contains(string(log), `"request_type":"exec"`) {
		t.Fatalf("collected tool-call log = %q %v", log, err)
	}
	if info, err := os.Stat(endpoint.LogPaths[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("collected log must be 0600: %v %v", info, err)
	}
	if _, err := dial(t, endpoint, identity); err == nil {
		t.Error("the bridge still accepts the key after Stop")
	}
	if status := h.daemon.Handle(ctx, Request{Op: OpStatus}); status.Grants != 0 {
		t.Errorf("daemon still holds %d grant(s) after release", status.Grants)
	}
	if err := client.Stop(ctx); err != nil {
		t.Errorf("second stop: %v", err)
	}
}

// If the bridge pod is gone, no session can be served (they lived only in its
// memory), so revocation is confirmed; but the evidence is lost, which must
// block evaluation rather than pass silently.
func TestStopConfirmsRevocationWhenTheBridgePodIsGoneButReportsLostEvidence(t *testing.T) {
	h := startDaemon(t, nil)
	ctx := context.Background()
	client := h.client(t)
	endpoint, err := client.Start(ctx, runnerSandbox{namespace: testNamespace})
	if err != nil {
		t.Fatal(err)
	}
	h.cluster.set(func(f *fakeCluster) { f.gone = true })
	err = client.Stop(ctx)
	if err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("stop = %v, want lost evidence reported", err)
	}
	if _, statErr := os.Stat(endpoint.IdentitySourceFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("revocation was confirmed, so the identity must be removed: %v", statErr)
	}
	if again := client.Stop(ctx); again == nil || again.Error() != err.Error() {
		t.Errorf("a retry must report the same loss, got %v", again)
	}
}

// A replacement pod with the same name but a new UID is a new process too.
func TestStopTreatsAReplacedPodAsGone(t *testing.T) {
	h := startDaemon(t, nil)
	ctx := context.Background()
	client := h.client(t)
	if _, err := client.Start(ctx, runnerSandbox{namespace: testNamespace}); err != nil {
		t.Fatal(err)
	}
	h.cluster.set(func(f *fakeCluster) { f.mute, f.podUID = true, "uid-2" })
	if err := client.Stop(ctx); err == nil || !strings.Contains(err.Error(), "lost") {
		t.Fatalf("stop = %v, want revocation confirmed with lost evidence", err)
	}
}

// A live pod that does not answer proves nothing: Stop must fail, keep the
// key, and succeed on a retry once the pod answers.
func TestStopFailsClosedWhenTheLivePodDoesNotAnswer(t *testing.T) {
	h := startDaemon(t, nil)
	ctx := context.Background()
	client := h.client(t)
	endpoint, err := client.Start(ctx, runnerSandbox{namespace: testNamespace})
	if err != nil {
		t.Fatal(err)
	}
	h.cluster.set(func(f *fakeCluster) { f.mute = true })
	if err := client.Stop(ctx); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("stop = %v, want unconfirmed revocation", err)
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); err != nil {
		t.Fatalf("the key must survive an unconfirmed revocation: %v", err)
	}
	h.cluster.set(func(f *fakeCluster) { f.mute = false })
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := os.Stat(endpoint.LogPaths[0]); err != nil {
		t.Errorf("retry did not collect the log: %v", err)
	}
}

// A grant the daemon refused never served anything, so Stop has nothing to
// confirm beyond the daemon reporting it absent.
func TestRefusedGrantStopsCleanly(t *testing.T) {
	h := startDaemon(t, func(context.Context, GrantRequest) (runner.Sandbox, error) {
		return nil, errors.New("refusing to attach to kubernetes pod: it is being deleted")
	})
	ctx := context.Background()
	client := h.client(t)
	if _, err := client.Start(ctx, runnerSandbox{namespace: testNamespace}); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("start = %v, want the attach refusal", err)
	}
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("stop after a refused grant: %v", err)
	}
	identity := filepath.Join(h.output, "task-1", "bridge", "id_ed25519")
	if _, err := os.Stat(identity); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("identity survived: %v", err)
	}
}

// If the grant request's fate is unknown (the call failed in transit), the
// runner still knows the ID it chose and can revoke it.
func TestGrantLostInTransitIsRevokedByTheRunnerChosenID(t *testing.T) {
	h := startDaemon(t, nil)
	ctx := context.Background()
	client := h.client(t)
	h.cluster.set(func(f *fakeCluster) { f.mute = true })
	if _, err := client.Start(ctx, runnerSandbox{namespace: testNamespace}); err == nil {
		t.Fatal("start succeeded through a failed exec")
	}
	h.cluster.set(func(f *fakeCluster) { f.mute = false })
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	h.cluster.mu.Lock()
	defer h.cluster.mu.Unlock()
	last := h.cluster.requests[len(h.cluster.requests)-1]
	if last.Op != OpRevoke || validateGrantID(last.GrantID) != nil {
		t.Fatalf("last request = %+v, want a revoke by the runner-chosen ID", last)
	}
}

func TestClientRefusesSandboxesItCannotHandTheBridgePod(t *testing.T) {
	h := startDaemon(t, nil)
	client := h.client(t)
	if _, err := client.Start(context.Background(), &podSandbox{}); err == nil {
		t.Error("accepted a sandbox with no Kubernetes identity")
	}
	other := h.client(t)
	if _, err := other.Start(context.Background(), runnerSandbox{namespace: "elsewhere"}); err == nil {
		t.Error("accepted a sandbox outside the bridge pod's namespace")
	}
}

func TestPreflightNeedsExactlyOneReadyBridgePod(t *testing.T) {
	h := startDaemon(t, nil)
	h.cluster.set(func(f *fakeCluster) { f.gone = true })
	err := Preflight(context.Background(), Options{BridgeType: "hermes-ssh", Transport: &KubeTransport{Namespace: testNamespace, Kubectl: h.cluster}, OutputDir: h.output,
		NewCredentials: func(dir string) (Credentials, error) { return hermesssh.NewCredentials(dir) }})
	if err == nil || !strings.Contains(err.Error(), "found 0") {
		t.Fatalf("preflight = %v, want no bridge pod reported", err)
	}
}
