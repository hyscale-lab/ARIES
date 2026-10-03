package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"golang.org/x/crypto/ssh"
)

// stubBridge writes one log and can be told to fail Start or Stop.
type stubBridge struct {
	dir       string
	startErr  error
	stopErr   error
	stopCalls int
}

func (b *stubBridge) Start(context.Context, runner.Sandbox) (core.ToolEndpoint, error) {
	log := filepath.Join(b.dir, "task-1", "bridge", "tool-calls.jsonl")
	if err := os.MkdirAll(filepath.Dir(log), 0o700); err != nil {
		return core.ToolEndpoint{}, err
	}
	if err := os.WriteFile(log, []byte(`{"request_type":"exec"}`+"\n"), 0o600); err != nil {
		return core.ToolEndpoint{}, err
	}
	if b.startErr != nil {
		return core.ToolEndpoint{}, b.startErr
	}
	return core.ToolEndpoint{Address: "10.0.0.5:40000", Network: "aries/" + testPod, LogPaths: []string{log}}, nil
}

func (b *stubBridge) Stop(context.Context) error {
	b.stopCalls++
	return b.stopErr
}

func stubDaemon(t *testing.T, bridge *stubBridge) *Daemon {
	t.Helper()
	daemon, err := NewDaemon(DaemonOptions{
		OutputDir: t.TempDir(), Backend: BackendKubernetes, Namespace: testNamespace,
		Attach: func(context.Context, GrantRequest) (runner.Sandbox, error) { return &podSandbox{}, nil },
		NewBridge: func(_ GrantRequest, dir string, _ ssh.Signer, _ ssh.PublicKey) (runner.ToolBridge, error) {
			bridge.dir = dir
			return bridge, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return daemon
}

func grantRequest(t *testing.T) *GrantRequest {
	t.Helper()
	credentials, err := newTestKey()
	if err != nil {
		t.Fatal(err)
	}
	return &GrantRequest{
		BridgeType: "hermes-ssh", AuthorizedKey: credentials,
		Sandbox: SandboxRef{Backend: BackendKubernetes, RunID: "run-1", TaskID: "task-1", Namespace: testNamespace,
			PodName: testPod, SandboxID: testSandboxID, Workdir: "/app"},
	}
}

func newTestKey() (string, error) {
	signer, err := newHostSigner()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

const grantA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestDaemonReleasesEvidenceOnlyAfterRevocation(t *testing.T) {
	bridge := &stubBridge{}
	daemon := stubDaemon(t, bridge)
	ctx := context.Background()
	granted := daemon.Handle(ctx, Request{Op: OpGrant, GrantID: grantA, Grant: grantRequest(t)})
	if granted.Error != "" || granted.Grant == nil || granted.Grant.HostKey == "" {
		t.Fatalf("grant = %+v", granted)
	}
	if early := daemon.Handle(ctx, Request{Op: OpCollect, GrantID: grantA}); early.Error == "" {
		t.Fatal("collect handed out evidence before revocation")
	}
	if early := daemon.Handle(ctx, Request{Op: OpRelease, GrantID: grantA}); early.Error == "" {
		t.Fatal("release discarded a live grant")
	}
	if revoked := daemon.Handle(ctx, Request{Op: OpRevoke, GrantID: grantA}); revoked.Error != "" || revoked.State != StateRevoked {
		t.Fatalf("revoke = %+v", revoked)
	}
	collected := daemon.Handle(ctx, Request{Op: OpCollect, GrantID: grantA})
	if collected.Error != "" || len(collected.Files) != 1 {
		t.Fatalf("collect = %+v", collected)
	}
	if again := daemon.Handle(ctx, Request{Op: OpRevoke, GrantID: grantA}); again.State != StateRevoked || bridge.stopCalls != 1 {
		t.Errorf("revoke is not idempotent: %+v after %d stops", again, bridge.stopCalls)
	}
	if released := daemon.Handle(ctx, Request{Op: OpRelease, GrantID: grantA}); released.Error != "" || released.State != StateAbsent {
		t.Fatalf("release = %+v", released)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(filepath.Dir(collected.Files[0])))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("release left the grant directory: %v", err)
	}
	if after := daemon.Handle(ctx, Request{Op: OpRevoke, GrantID: grantA}); after.State != StateAbsent {
		t.Errorf("released grant reports %q", after.State)
	}
}

// A partial start whose own cleanup failed may still be serving, so it stays
// registered for the runner's revoke to retry rather than being forgotten.
func TestDaemonKeepsAnUnconfirmedPartialStartForRevocation(t *testing.T) {
	bridge := &stubBridge{startErr: errors.New("listen failed"), stopErr: errors.New("drain timed out")}
	daemon := stubDaemon(t, bridge)
	ctx := context.Background()
	response := daemon.Handle(ctx, Request{Op: OpGrant, GrantID: grantA, Grant: grantRequest(t)})
	if response.Error == "" || response.State != StateActive {
		t.Fatalf("grant = %+v, want an error with the grant still active", response)
	}
	if revoke := daemon.Handle(ctx, Request{Op: OpRevoke, GrantID: grantA}); revoke.Error == "" {
		t.Fatal("revoke confirmed while the bridge still fails to stop")
	}
	bridge.stopErr = nil
	if revoke := daemon.Handle(ctx, Request{Op: OpRevoke, GrantID: grantA}); revoke.Error != "" || revoke.State != StateRevoked {
		t.Fatalf("revoke retry = %+v", revoke)
	}
}

// A partial start that cleaned up after itself is forgotten entirely.
func TestDaemonForgetsACleanPartialStart(t *testing.T) {
	daemon := stubDaemon(t, &stubBridge{startErr: errors.New("listen failed")})
	response := daemon.Handle(context.Background(), Request{Op: OpGrant, GrantID: grantA, Grant: grantRequest(t)})
	if response.Error == "" || response.State != StateAbsent {
		t.Fatalf("grant = %+v, want an error with the grant absent", response)
	}
}

func TestDaemonRejectsMalformedGrants(t *testing.T) {
	cases := map[string]func(*Request){
		"bad grant ID":        func(r *Request) { r.GrantID = "../../etc" },
		"other namespace":     func(r *Request) { r.Grant.Sandbox.Namespace = "kube-system" },
		"other backend":       func(r *Request) { r.Grant.Sandbox.Backend = BackendDocker },
		"unknown backend":     func(r *Request) { r.Grant.Sandbox.Backend = "nomad" },
		"listen on a name":    func(r *Request) { r.Grant.ListenHost = "bridge.example" },
		"listen on IPv6":      func(r *Request) { r.Grant.ListenHost = "::1" },
		"unsupported bridge":  func(r *Request) { r.Grant.BridgeType = "openclaw-e2b" },
		"no authorized key":   func(r *Request) { r.Grant.AuthorizedKey = "" },
		"two authorized keys": func(r *Request) { r.Grant.AuthorizedKey += "\n" + r.Grant.AuthorizedKey },
		"garbage key":         func(r *Request) { r.Grant.AuthorizedKey = "ssh-ed25519 not-base64" },
		"missing pod":         func(r *Request) { r.Grant.Sandbox.PodName = "" },
		"missing grant":       func(r *Request) { r.Grant = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bridge := &stubBridge{}
			daemon := stubDaemon(t, bridge)
			request := Request{Op: OpGrant, GrantID: grantA, Grant: grantRequest(t)}
			mutate(&request)
			if response := daemon.Handle(context.Background(), request); response.Error == "" {
				t.Fatal("accepted a malformed grant")
			}
			if bridge.dir != "" {
				t.Error("built a bridge for a malformed grant")
			}
		})
	}
}

func TestDaemonRejectsADuplicateGrantID(t *testing.T) {
	daemon := stubDaemon(t, &stubBridge{})
	ctx := context.Background()
	if first := daemon.Handle(ctx, Request{Op: OpGrant, GrantID: grantA, Grant: grantRequest(t)}); first.Error != "" {
		t.Fatal(first.Error)
	}
	if second := daemon.Handle(ctx, Request{Op: OpGrant, GrantID: grantA, Grant: grantRequest(t)}); second.Error == "" {
		t.Fatal("the same grant ID was granted twice")
	}
}

func TestDaemonCloseRevokesLiveGrantsAndRefusesNewOnes(t *testing.T) {
	bridge := &stubBridge{}
	daemon := stubDaemon(t, bridge)
	ctx := context.Background()
	daemon.Handle(ctx, Request{Op: OpGrant, GrantID: grantA, Grant: grantRequest(t)})
	if err := daemon.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if bridge.stopCalls != 1 {
		t.Errorf("close stopped the bridge %d times", bridge.stopCalls)
	}
	if late := daemon.Handle(ctx, Request{Op: OpGrant, GrantID: strings.Repeat("b", 32), Grant: grantRequest(t)}); late.Error == "" {
		t.Error("granted after close")
	}
}

// collect writes only the expected log names and never follows a link.
func TestAppendLogRefusesUnexpectedNamesAndLinks(t *testing.T) {
	dir := t.TempDir()
	archive := tar.NewWriter(&bytes.Buffer{})
	other := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(other, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendLog(archive, other); err == nil {
		t.Error("collected a file that is not a bridge log")
	}
	link := filepath.Join(dir, "tool-calls.jsonl")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := appendLog(archive, link); err == nil {
		t.Error("followed a symbolic link into another file")
	}
}
