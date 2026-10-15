package bridge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	managed "github.com/hyscale-lab/aries/pkg/bridge"
	grpcbridge "github.com/hyscale-lab/aries/pkg/bridge/grpc"
	sshbridge "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/hermes"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// ServeChild selects execution and native adapters; deployment resolves addresses
// for consumers outside the child. The child only reports each bound port.
func ServeChild(ctx context.Context, c managed.LaunchConfig, logger *logrus.Logger) error {
	var backend target.Backend
	var closeBackend func() error
	switch c.Backend {
	case "docker":
		d, err := docker.New(docker.Options{Socket: c.BackendEndpoint, Logger: logger})
		if err != nil {
			return err
		}
		backend = d
		closeBackend = d.Close
	default:
		return errors.New("unsupported bridge execution backend")
	}
	defer closeBackend()
	resolveListen := func(context.Context) (core.BridgeListen, error) { return c.Listen, nil }
	var dialect sshbridge.Dialect
	switch c.BridgeType {
	case "hermes-ssh":
		dialect = hermes.Dialect{}
	case "openclaw-ssh":
		dialect = openclaw.Dialect{}
	case "hermes-grpc":
		return managed.Serve(ctx, managed.ServeOptions{Config: c, Backend: backend, NewNative: func(sandboxID, outputDir string) (managed.NativeServer, error) {
			return grpcbridge.New(grpcbridge.Options{SandboxID: sandboxID, ResolveListen: resolveListen, OutputDir: outputDir, Logger: logger, OmitRawLog: !c.RetainRawLog})
		}})
	default:
		return errors.New("unsupported bridge protocol")
	}
	// One host key per child process: every SSH listener of the run presents it.
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return err
	}
	return managed.Serve(ctx, managed.ServeOptions{Config: c, Backend: backend, NewNative: func(sandboxID, outputDir string) (managed.NativeServer, error) {
		return sshbridge.New(sshbridge.Options{Dialect: dialect, HostSigner: signer, SandboxID: sandboxID, ResolveListen: resolveListen, OutputDir: outputDir, Logger: logger, OmitRawLog: !c.RetainRawLog})
	}})
}

var _ managed.NativeServer = (*sshbridge.Manager)(nil)
var _ managed.NativeServer = (*grpcbridge.Manager)(nil)
