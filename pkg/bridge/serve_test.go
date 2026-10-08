package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

type bootstrapBackend struct{ target.Backend }

func (bootstrapBackend) ValidateBridgeTarget(context.Context, core.BridgeTarget) error { return nil }

type cleanupNative struct {
	target  target.Executor
	root    string
	stopErr error
}

func (n *cleanupNative) StartTarget(_ context.Context, borrowed target.Executor) (core.ToolEndpoint, error) {
	n.target = borrowed
	return core.ToolEndpoint{Address: "0.0.0.0:2222", Username: "aries", Protocol: "ssh"}, nil
}
func (n *cleanupNative) Stop(ctx context.Context) error {
	if _, err := n.target.ExecStream(ctx, core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard); err == nil {
		return errors.New("borrowed admission remained open")
	}
	if n.stopErr != nil {
		return n.stopErr
	}
	if err := os.MkdirAll(n.root, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(n.root, "tool-calls.jsonl"), []byte("finalized evidence"), 0600)
}
func TestServeSharesHostSignerAndSurvivesIndependentReleases(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var keys []string
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, ServeOptions{Config: LaunchConfig{RunID: "run", Backend: "fixture", OutputDir: root}, Backend: bootstrapBackend{}, NewNative: func(signer ssh.Signer, id, output string) (NativeServer, error) {
			mu.Lock()
			keys = append(keys, string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
			mu.Unlock()
			if output != filepath.Join(root, id) {
				return nil, errors.New("cross-sandbox evidence path")
			}
			return &cleanupNative{root: output}, nil
		}}, listener)
	}()
	conn, client, err := control.NewClient(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	call, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	register := func(id string) {
		t.Helper()
		a, err := client.RegisterSandbox(call, &v1.RegisterSandboxRequest{SandboxId: id, Target: &v1.Target{RuntimeId: "runtime-" + id, Workdir: "/app"}, Metadata: &v1.Metadata{TaskId: "task"}})
		if err != nil || a.State != v1.State_READY {
			t.Fatal(a, err)
		}
	}
	release := func(id string) {
		t.Helper()
		a, err := client.ReleaseSandbox(call, &v1.SandboxRequest{SandboxId: id})
		if err != nil || a.State != v1.State_RELEASED || len(a.Artifacts) != 1 || a.Artifacts[0].Size != int64(len("finalized evidence")) {
			t.Fatal(a, err)
		}
	}
	register("a")
	register("b")
	release("a")
	b, err := client.GetSandbox(call, &v1.SandboxRequest{SandboxId: "b"})
	if err != nil || b.State != v1.State_READY {
		t.Fatal(b, err)
	}
	release("b")
	register("c")
	release("c")
	select {
	case err := <-done:
		t.Fatal("idle service exited", err)
	default:
	}
	mu.Lock()
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatal("signer changed across sessions")
	}
	mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("service failed to exit")
	}
	for _, id := range []string{"a", "b", "c"} {
		entries, err := os.ReadDir(filepath.Join(root, id))
		if err != nil || len(entries) != 1 || entries[0].Name() != "tool-calls.jsonl" {
			t.Fatal("unexpected staged credentials", entries, err)
		}
	}
}
func TestServeRequiresRunAndInjectedFactory(t *testing.T) {
	if err := Serve(context.Background(), ServeOptions{}); err == nil {
		t.Fatal("missing run/factory admitted")
	}
}
