// Package testfixture supplies a service host signer for native SSH tests.
package testfixture

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

type Server struct{ Manager *bridgessh.Manager }

func New(t *testing.T, options bridgessh.Options) *Server {
	t.Helper()
	if options.HostSigner == nil {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		options.HostSigner, err = ssh.NewSignerFromKey(key)
		if err != nil {
			t.Fatal(err)
		}
	}
	if options.SandboxID == "" {
		options.SandboxID = "test-sandbox"
	}
	if options.ResolveListen == nil {
		options.ResolveListen = func(context.Context) (core.BridgeListen, error) {
			return core.BridgeListen{BindHost: "127.0.0.1"}, nil
		}
	}
	manager, err := bridgessh.New(options)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: manager}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Stop(ctx); err != nil {
			t.Errorf("native fixture cleanup: %v", err)
		}
	})
	return server
}

func (server *Server) StartTarget(ctx context.Context, executor target.Executor) (core.ToolEndpoint, error) {
	return server.Manager.StartTarget(ctx, executor)
}
func (server *Server) Stop(ctx context.Context) error { return server.Manager.Stop(ctx) }
