package hermesssh

import (
	"context"
	"errors"
	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
	"net"
	"os"
	"strings"
	"testing"
)

func loopbackListen(context.Context) (core.BridgeListen, error) {
	return core.BridgeListen{BindHost: "127.0.0.1", AdvertiseHost: "127.0.0.1"}, nil
}

func TestListenSeparatesBindingAndAdvertisement(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	manager.resolveListen = func(context.Context) (core.BridgeListen, error) {
		return core.BridgeListen{BindHost: "127.0.0.1", AdvertiseHost: "127.0.0.2"}, nil
	}
	endpoint, err := manager.Start(context.Background(), &testSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host != "127.0.0.2" || port == "0" {
		t.Fatalf("advertised endpoint = %q, %v", endpoint.Address, err)
	}
	bound := manager.active.listener.Addr().String()
	_, boundPort, _ := net.SplitHostPort(bound)
	if boundPort != port {
		t.Fatalf("bound %s advertised %s", bound, endpoint.Address)
	}
	known, err := os.ReadFile(manager.active.knownSource)
	if err != nil || !strings.HasPrefix(string(known), "[127.0.0.2]:"+port+" ") {
		t.Fatalf("known hosts = %q, %v", known, err)
	}
	conn, err := net.Dial("tcp4", bound)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("tcp4", bound); err == nil {
		_ = conn.Close()
		t.Fatal("listener remained reachable")
	}
}

func TestListenRejectsInvalidSettingsBeforeGrant(t *testing.T) {
	for _, setting := range []core.BridgeListen{
		{BindHost: "localhost", AdvertiseHost: "127.0.0.1"},
		{BindHost: "127.0.0.1", AdvertiseHost: "localhost"},
		{BindHost: "127.0.0.1", AdvertiseHost: "0.0.0.0"},
		{BindHost: "::1", AdvertiseHost: "::1"},
		{BindHost: "192.0.2.1", AdvertiseHost: "192.0.2.1"},
	} {
		t.Run(setting.BindHost+"/"+setting.AdvertiseHost, func(t *testing.T) {
			manager := newTestManager(t, t.TempDir())
			manager.resolveListen = func(context.Context) (core.BridgeListen, error) { return setting, nil }
			if _, err := manager.Start(context.Background(), &testSandbox{}); err == nil {
				t.Fatal("invalid settings accepted")
			}
			if manager.active != nil {
				t.Fatal("failed settings left an active grant")
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestConcurrentGrantsRejectAnotherTasksKey(t *testing.T) {
	first, second := newTestManager(t, t.TempDir()), newTestManager(t, t.TempDir())
	one, err := first.Start(context.Background(), &testSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	two, err := second.Start(context.Background(), &testSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if one.Address == two.Address {
		t.Fatal("tasks share a listener")
	}
	own, other := clientConfig(t, one), clientConfig(t, two)
	wrong := *own
	wrong.Auth = other.Auth
	if conn, err := ssh.Dial("tcp", one.Address, &wrong); err == nil {
		_ = conn.Close()
		t.Fatal("another task key authenticated")
	}
	conn, err := ssh.Dial("tcp", one.Address, own)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if err := first.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	conn, err = ssh.Dial("tcp", two.Address, other)
	if err != nil {
		t.Fatalf("revoking first task revoked second: %v", err)
	}
	_ = conn.Close()
}

func TestListenResolutionFailureCreatesNoGrant(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	cause := errors.New("task environment unavailable")
	manager.resolveListen = func(context.Context) (core.BridgeListen, error) { return core.BridgeListen{}, cause }
	if _, err := manager.Start(context.Background(), &testSandbox{}); !errors.Is(err, cause) {
		t.Fatalf("resolve failure = %v", err)
	}
	if manager.active != nil {
		t.Fatal("failed resolution created a grant")
	}
}
