package ssh

import (
	"context"
	"errors"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
	"net"
	"testing"
)

func TestListenReturnsLocalBindingForDeploymentResolution(t *testing.T) {
	manager := newContractManager(t, t.TempDir())
	manager.resolveListen = func(context.Context) (core.BridgeListen, error) {
		return core.BridgeListen{BindHost: "127.0.0.1"}, nil
	}
	endpoint, err := manager.StartTarget(context.Background(), &contractSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host != "127.0.0.1" || port == "0" {
		t.Fatalf("local endpoint = %q, %v", endpoint.Address, err)
	}
	if endpoint.Address != manager.active.listener.Addr().String() {
		t.Fatal("endpoint does not report actual local binding")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp", endpoint.Address); err == nil {
		_ = conn.Close()
		t.Fatal("listener remained reachable")
	}
}

func TestListenRejectsInvalidSettingsBeforeGrant(t *testing.T) {
	for _, setting := range []core.BridgeListen{
		{BindHost: "localhost"}, {BindHost: "127.0.0.1", BindPort: -1},
		{BindHost: "127.0.0.1", BindPort: 65536}, {BindHost: "192.0.2.1"},
	} {
		t.Run(setting.BindHost, func(t *testing.T) {
			manager := newContractManager(t, t.TempDir())
			manager.resolveListen = func(context.Context) (core.BridgeListen, error) { return setting, nil }
			if _, err := manager.StartTarget(context.Background(), &contractSandbox{}); err == nil {
				t.Fatal("invalid settings accepted")
			}
			if manager.active != nil {
				t.Fatal("failed settings left active grant")
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentListenersAcceptNoCredentialsAndRevokeIndependently(t *testing.T) {
	first, second := newContractManager(t, t.TempDir()), newContractManager(t, t.TempDir())
	second.hostSigner = first.hostSigner
	one, err := first.StartTarget(context.Background(), &contractSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	two, err := second.StartTarget(context.Background(), &contractSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if one.Address == two.Address {
		t.Fatal("sandboxes share a listener")
	}
	var presented []string
	for _, endpoint := range []core.ToolEndpoint{one, two, one} {
		config := bridgeClientConfig(t, endpoint)
		config.HostKeyCallback = func(_ string, _ net.Addr, key ssh.PublicKey) error {
			presented = append(presented, string(key.Marshal()))
			return nil
		}
		conn, err := ssh.Dial("tcp", endpoint.Address, config)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}
	if presented[0] != presented[1] || presented[1] != presented[2] {
		t.Fatal("service host key changed between listeners or reconnect")
	}
	if err := first.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err := ssh.Dial("tcp", two.Address, bridgeClientConfig(t, two))
	if err != nil {
		t.Fatalf("revoking first revoked second: %v", err)
	}
	_ = conn.Close()
}

func TestListenResolutionFailureCreatesNoGrant(t *testing.T) {
	manager := newContractManager(t, t.TempDir())
	cause := errors.New("task environment unavailable")
	manager.resolveListen = func(context.Context) (core.BridgeListen, error) { return core.BridgeListen{}, cause }
	if _, err := manager.StartTarget(context.Background(), &contractSandbox{}); !errors.Is(err, cause) {
		t.Fatalf("resolve failure = %v", err)
	}
	if manager.active != nil {
		t.Fatal("failed resolution created a grant")
	}
}
