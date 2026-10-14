package bridge

import (
	"context"
	"errors"
	managed "github.com/hyscale-lab/aries/pkg/bridge"
	sshbridge "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/hermes"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/apptainer"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/hyscale-lab/aries/pkg/deployment/kubernetes"
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
	case "kubernetes":
		// The endpoint is the sandbox namespace; the pod's service account
		// authorizes kubectl.
		k, err := kubernetes.New(kubernetes.Options{Namespace: c.BackendEndpoint, Logger: logger})
		if err != nil {
			return err
		}
		backend = k
		closeBackend = k.Close
	case "apptainer":
		// The endpoint is the state directory holding the sandboxes' records.
		a, err := apptainer.New(apptainer.Options{StateDir: c.BackendEndpoint, Logger: logger})
		if err != nil {
			return err
		}
		backend = a
		closeBackend = a.Close
	default:
		return errors.New("unsupported bridge execution backend")
	}
	defer closeBackend()
	return managed.Serve(ctx, managed.ServeOptions{Config: c, Backend: backend, NewNative: func(signer ssh.Signer, sandboxID, outputDir string) (managed.NativeServer, error) {
		options := sshbridge.Options{HostSigner: signer, SandboxID: sandboxID, ResolveListen: func(context.Context) (core.BridgeListen, error) { return c.Listen, nil }, OutputDir: outputDir, Logger: logger, OmitRawLog: !c.RetainRawLog}
		switch c.BridgeType {
		case "hermes-ssh":
			options.Dialect = hermes.Dialect{}
			return sshbridge.New(options)
		case "openclaw-ssh":
			return openclaw.New(options)
		default:
			return nil, errors.New("unsupported bridge protocol")
		}
	}})
}

var _ managed.NativeServer = (*sshbridge.Manager)(nil)

var _ managed.NativeServer = (*openclaw.Server)(nil)
