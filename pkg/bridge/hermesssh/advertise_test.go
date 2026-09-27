package hermesssh

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// gatewaylessSandbox is a sandbox with no Docker network gateway, as on
// Kubernetes, where the "gateway" would be the task pod's own IP.
type gatewaylessSandbox struct{ testSandbox }

func (*gatewaylessSandbox) NetworkGateway(context.Context) (string, error) {
	return "", errors.New("no Docker network gateway on this backend")
}

// In advertised mode the bridge must not ask for a network gateway, must
// accept connections on the advertised address, and must record the host key
// against the address Hermes actually dials.
func TestAdvertisedModeBindsAllInterfacesAndNamesTheAdvertisedHost(t *testing.T) {
	manager, err := New(Options{OutputDir: t.TempDir(), CleanupTimeout: 5 * time.Second, AdvertiseHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	endpoint, err := manager.Start(ctx, &gatewaylessSandbox{})
	if err != nil {
		t.Fatalf("advertised mode consulted the network gateway: %v", err)
	}
	defer manager.Stop(ctx)

	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host != "127.0.0.1" || port == "" {
		t.Fatalf("endpoint address = %q, want the advertised host", endpoint.Address)
	}
	// The listener is on every interface, not only the advertised one.
	if bound := manager.active.listener.Addr().String(); !strings.HasPrefix(bound, "0.0.0.0:") {
		t.Errorf("listener bound to %s, want 0.0.0.0", bound)
	}

	client, err := ssh.Dial("tcp", endpoint.Address, clientConfig(t, endpoint))
	if err != nil {
		t.Fatalf("dial the advertised address: %v", err)
	}
	_ = client.Close()

	known, err := os.ReadFile(manager.active.knownSource)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(known), "[127.0.0.1]:"+port+" ") {
		t.Errorf("known_hosts = %q, want it keyed by the advertised address", known)
	}
}

// Without an advertised host the Docker behaviour is unchanged: the gateway is
// both the bound and the advertised address, so a sandbox with no gateway is
// still refused.
func TestDockerModeStillRequiresTheNetworkGateway(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := manager.Start(ctx, &gatewaylessSandbox{}); err == nil {
		_ = manager.Stop(ctx)
		t.Fatal("Docker mode started without a network gateway")
	}
}
