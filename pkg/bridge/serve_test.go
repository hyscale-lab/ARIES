package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	sshcredentials "github.com/hyscale-lab/aries/pkg/bridge/ssh/credentials"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/credentials"
)

func TestLeaseFailureStillBoundsChildCollectionAndExit(t *testing.T) {
	service, err := control.NewServer(control.Config{InstanceID: "instance", Token: strings.Repeat("x", 32), MaxLease: time.Second,
		Assign: func(context.Context, *v1.AssignSandboxRequest) (*v1.Endpoint, error) {
			return &v1.Endpoint{Host: "127.0.0.1", Port: 22}, nil
		},
		Revoke: func(context.Context) ([]*v1.Artifact, error) { return nil, errors.New("native cleanup unconfirmed") },
	})
	if err != nil {
		t.Fatal(err)
	}
	d := core.BridgeTarget{Version: 1, RunID: "run", TaskID: "task", OccurrenceID: "occurrence", Backend: "docker", RuntimeID: "runtime", RuntimeName: "sandbox", Workdir: "/app", MaxInputBytes: 16 << 20, MaxOutputBytes: 1 << 30, ExpectedLabels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run", "aries.task": "task"}}
	_, err = service.AssignSandbox(context.Background(), &v1.AssignSandboxRequest{InstanceId: "instance", AssignmentId: "assignment", ProtocolVersion: 1, Target: control.TargetToProto(d), LeaseMillis: 10, CredentialId: "ssh"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-service.Revoking():
	case <-time.After(time.Second):
		t.Fatal("lease did not close admission")
	}
	if err = awaitCollectionExit(service, 10*time.Millisecond, make(chan error)); err == nil {
		t.Fatal("failed native cleanup became successful child exit")
	}
	a, err := service.GetAssignment(context.Background(), &v1.AssignmentRequest{InstanceId: "instance", AssignmentId: "assignment"})
	if err != nil || a.State != v1.State_REVOKING {
		t.Fatalf("unconfirmed native cleanup acknowledged: %+v %v", a, err)
	}
	select {
	case <-service.Done():
		t.Fatal("failed native cleanup produced revocation acknowledgement")
	default:
	}
}

func TestServeExitErasesPrivateBootstrapAndPreservesEvidence(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"host.key", "authorized.pub", "server.key", "token", "tool-calls.jsonl"} {
		if err := os.WriteFile(name, []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Missing CA fails before exposing control, exercising unconditional exit cleanup.
	if err := Serve(context.Background(), ServeOptions{}); err == nil {
		t.Fatal("missing bootstrap CA accepted")
	}
	for _, name := range []string{"host.key", "authorized.pub", "server.key", "token"} {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("credential %s survived exit: %v", name, err)
		}
	}
	if content, err := os.ReadFile("tool-calls.jsonl"); err != nil || string(content) != "private" {
		t.Fatalf("evidence removed: %q %v", content, err)
	}
}

func TestCredentialEraseAttemptsAllFilesAndReportsFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("host.key", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("host.key", "unexpected"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("token", []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := eraseStagedCredentials("host.key", "token"); err == nil {
		t.Fatal("credential cleanup failure hidden")
	}
	if _, err := os.Lstat("token"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later credential not erased: %v", err)
	}
}

type bootstrapBackend struct{ target.Backend }

func (bootstrapBackend) ValidateBridgeTarget(context.Context, core.BridgeTarget) error { return nil }

type cleanupNative struct {
	target  target.Executor
	stopErr error
}

func (n *cleanupNative) StartTarget(_ context.Context, target target.Executor) (core.ToolEndpoint, error) {
	n.target = target
	return core.ToolEndpoint{Address: "127.0.0.1:2222", Username: "aries", Protocol: "ssh"}, nil
}
func (n *cleanupNative) Stop(ctx context.Context) error {
	if _, err := n.target.ExecStream(ctx, core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard); err == nil {
		return errors.New("borrowed admission remained open during native cleanup")
	}
	if n.stopErr != nil {
		return n.stopErr
	}
	if err := os.MkdirAll("evidence/task/bridge", 0700); err != nil {
		return err
	}
	return os.WriteFile("evidence/task/bridge/tool-calls.jsonl", []byte("finalized evidence"), 0600)
}

func TestNativeCleanupClosesAdmissionFinalizesEvidenceAndErasesCredentials(t *testing.T) {
	for _, test := range []struct {
		name    string
		stopErr error
	}{
		{name: "complete"},
		{name: "failed", stopErr: errors.New("native cleanup deliberately unconfirmed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			keys, err := newControlCredentials()
			if err != nil {
				t.Fatal(err)
			}
			hostPrivate, _, err := sshcredentials.GenerateIdentity()
			if err != nil {
				t.Fatal(err)
			}
			_, clientPublic, err := sshcredentials.GenerateIdentity()
			if err != nil {
				t.Fatal(err)
			}
			token := strings.Repeat("t", 32)
			for name, contents := range map[string][]byte{"ca.pem": keys.CA, "server.pem": keys.ServerCert, "server.key": keys.ServerKey, "token": []byte(token), "host.key": hostPrivate, "authorized.pub": ssh.MarshalAuthorizedKey(clientPublic), "tool-calls.jsonl": []byte("evidence")} {
				if err := os.WriteFile(name, contents, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			done := make(chan error, 1)
			go func() {
				done <- serve(ctx, ServeOptions{Config: LaunchConfig{InstanceID: "instance", OutputDir: "evidence"}, Backend: bootstrapBackend{}, NewNative: func(*sshcredentials.Credentials) (NativeServer, error) {
					return &cleanupNative{stopErr: test.stopErr}, nil
				}, CollectionTimeout: 100 * time.Millisecond}, listener)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
				}
			})
			tls, err := controlTLS(keys.CA, keys.ClientCert, keys.ClientKey, false)
			if err != nil {
				t.Fatal(err)
			}
			conn, client, err := control.NewClient(address, "instance", token, credentials.NewTLS(tls))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			d := core.BridgeTarget{Version: 1, RunID: "run", TaskID: "task", OccurrenceID: "occurrence", Backend: "docker", RuntimeID: "runtime", RuntimeName: "sandbox", Workdir: "/app", MaxInputBytes: 16 << 20, MaxOutputBytes: 1 << 30, ExpectedLabels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run", "aries.task": "task"}}
			call, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			a, err := client.AssignSandbox(call, &v1.AssignSandboxRequest{InstanceId: "instance", AssignmentId: "assignment", ProtocolVersion: 1, Target: control.TargetToProto(d), LeaseMillis: 1000, CredentialId: "ssh"})
			if err != nil || a.State != v1.State_READY {
				t.Fatalf("assignment failed: %v %v", a, err)
			}
			a, err = client.RevokeAssignment(call, &v1.AssignmentRequest{InstanceId: "instance", AssignmentId: "assignment"})
			wantState := v1.State_REVOKED
			if test.stopErr != nil {
				wantState = v1.State_REVOKING
			}
			if err != nil || a.State != wantState {
				t.Fatalf("unexpected native cleanup result: %v %v", a, err)
			}
			if test.stopErr == nil && (len(a.Artifacts) != 1 || a.Artifacts[0].Status != "complete" || a.Artifacts[0].Size != int64(len("finalized evidence"))) {
				t.Fatalf("evidence was not finalized after native cleanup: %v", a.Artifacts)
			}
			for _, name := range []string{"host.key", "authorized.pub"} {
				if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("SSH credential survived revoke: %s %v", name, err)
				}
			}
			select {
			case err := <-done:
				if (err != nil) != (test.stopErr != nil) {
					t.Fatalf("service exit did not reflect native cleanup result: %v", err)
				}
				done <- err
			case <-time.After(time.Second):
				t.Fatal("native cleanup did not bound service exit")
			}
			for _, name := range []string{"server.key", "token"} {
				if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("control credential survived exit: %s %v", name, err)
				}
			}
			if contents, err := os.ReadFile("tool-calls.jsonl"); err != nil || string(contents) != "evidence" {
				t.Fatalf("evidence lost: %q %v", contents, err)
			}
		})
	}
}
