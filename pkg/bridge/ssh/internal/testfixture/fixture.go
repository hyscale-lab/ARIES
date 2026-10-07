// Package testfixture stages controller-owned credentials for native SSH tests.
// Production construction must remain in application wiring.
package testfixture

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/credentials"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

// Server separates controller-owned client credentials from server authority.
type Server struct {
	Manager  *bridgessh.Manager
	identity string
}

func New(t *testing.T, options bridgessh.Options) *Server {
	t.Helper()
	clientPrivate, clientPublic, err := credentials.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	hostPrivate, _, err := credentials.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	options.Credentials, err = credentials.ParseCredentials(hostPrivate, ssh.MarshalAuthorizedKey(clientPublic))
	if err != nil {
		t.Fatal(err)
	}
	if options.ResolveListen == nil {
		options.ResolveListen = func(context.Context) (core.BridgeListen, error) {
			return core.BridgeListen{BindHost: "127.0.0.1", AdvertiseHost: "127.0.0.1"}, nil
		}
	}
	manager, err := bridgessh.New(options)
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(identity, clientPrivate, 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{Manager: manager, identity: identity}
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
	endpoint, err := server.Manager.StartTarget(ctx, executor)
	if err == nil {
		endpoint.IdentitySourceFile = server.identity
	}
	return endpoint, err
}

func (server *Server) Stop(ctx context.Context) error {
	if err := server.Manager.Stop(ctx); err != nil {
		return err
	}
	// Like the controller, retain the identity until native shutdown is confirmed.
	if err := os.Remove(server.identity); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
