package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
	grpcbridge "github.com/hyscale-lab/aries/pkg/bridge/grpc"
	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	sshbridge "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// childTarget accepts every command. OpenClaw reads the canonical workspace
// root from its setup command before it starts SSH.
type childTarget struct {
	target.Executor // file operations are unused by SSH
}

func (childTarget) ContainerID() string   { return "container" }
func (childTarget) ContainerName() string { return "sandbox" }
func (childTarget) RunID() string         { return "run" }
func (childTarget) TaskID() string        { return "task" }
func (childTarget) Workdir() string       { return "/work" }
func (childTarget) ExecStream(_ context.Context, _ core.Command, _ io.Reader, stdout, _ io.Writer) (core.CommandResult, error) {
	_, err := io.WriteString(stdout, "/work\n")
	return core.CommandResult{}, err
}

func startChildNative(t *testing.T, child func(string, string) (managed.NativeServer, error), sandboxID string) string {
	t.Helper()
	native, err := child(sandboxID, filepath.Join(t.TempDir(), sandboxID))
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := native.StartTarget(context.Background(), childTarget{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := native.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return presentedHostKey(t, endpoint.Address)
}

// presentedHostKey stops the handshake once the listener has shown its key.
func presentedHostKey(t *testing.T, address string) string {
	t.Helper()
	var presented string
	_, err := ssh.Dial("tcp", address, &ssh.ClientConfig{User: "aries", HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		presented = string(key.Marshal())
		return errors.New("host key captured")
	}})
	if presented == "" {
		t.Fatalf("no host key from %s: %v", address, err)
	}
	return presented
}

func TestChildPresentsOneHostKeyOnEverySSHListener(t *testing.T) {
	for _, bridgeType := range []string{"hermes-ssh", "openclaw-ssh"} {
		t.Run(bridgeType, func(t *testing.T) {
			config := managed.LaunchConfig{BridgeType: bridgeType, Listen: core.BridgeListen{BindHost: "127.0.0.1"}}
			child, err := newNative(config, logrus.New())
			if err != nil {
				t.Fatal(err)
			}
			first := startChildNative(t, child, "sandbox-a")
			if startChildNative(t, child, "sandbox-b") != first {
				t.Fatal("listeners of one child present different host keys")
			}
			other, err := newNative(config, logrus.New())
			if err != nil {
				t.Fatal(err)
			}
			if startChildNative(t, other, "sandbox-c") == first {
				t.Fatal("two children present the same host key")
			}
		})
	}
}

func TestChildSelectsTheNativeServerOfTheBridgeType(t *testing.T) {
	for _, bridgeType := range []string{"hermes-ssh", "openclaw-ssh", "hermes-grpc"} {
		child, err := newNative(managed.LaunchConfig{BridgeType: bridgeType}, logrus.New())
		if err != nil {
			t.Fatal(err)
		}
		native, err := child("sandbox", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		_, manager := native.(*sshbridge.Manager)
		_, server := native.(*openclaw.Server)
		_, grpcManager := native.(*grpcbridge.Manager)
		if manager != (bridgeType == "hermes-ssh") || server != (bridgeType == "openclaw-ssh") || grpcManager != (bridgeType == "hermes-grpc") {
			t.Fatalf("%s selected %T", bridgeType, native)
		}
	}
	child, err := newNative(managed.LaunchConfig{BridgeType: "fixture-ssh"}, logrus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child("sandbox", t.TempDir()); err == nil {
		t.Fatal("bridge type without native server accepted")
	}
}

func TestChildPropagatesReplayCapture(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(strconv.FormatBool(capture), func(t *testing.T) {
			config := managed.LaunchConfig{BridgeType: "hermes-grpc", Listen: core.BridgeListen{BindHost: "127.0.0.1"}, CaptureReplay: capture}
			child, err := newNative(config, logrus.New())
			if err != nil {
				t.Fatal(err)
			}
			native, err := child("sandbox", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			endpoint, err := native.StartTarget(context.Background(), childTarget{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = native.Stop(context.Background()) })
			connection, err := grpc.NewClient(endpoint.Address, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if _, err := sandboxv1.NewSandboxClient(connection).Exec(context.Background(), &sandboxv1.ExecRequest{Script: "pwd"}); err != nil {
				t.Fatal(err)
			}
			if err := native.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			audit, err := os.ReadFile(endpoint.LogPaths[0])
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(audit, []byte(`"response_stdout"`)) != capture {
				t.Fatalf("capture=%v, audit=%s", capture, audit)
			}
		})
	}
}
