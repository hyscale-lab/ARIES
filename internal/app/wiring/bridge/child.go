package bridge

import (
	"context"
	"errors"
	"net"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
	sshbridge "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	sshcredentials "github.com/hyscale-lab/aries/pkg/bridge/ssh/credentials"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/hermes"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/sirupsen/logrus"
)

// ServeChild opens an independent infrastructure client inside aries-bridge.
func ServeChild(ctx context.Context, c managed.LaunchConfig, logger *logrus.Logger) error {
	var backend target.Backend
	var closeBackend func() error
	switch c.Backend {
	case "docker":
		d, err := docker.New(docker.Options{Socket: c.DockerSocket, Logger: logger})
		if err != nil {
			return err
		}
		backend = d
		closeBackend = d.Close
	default:
		return errors.New("unsupported bridge execution backend")
	}
	defer closeBackend()
	listen := func(context.Context) (core.BridgeListen, error) {
		value := c.Listen
		if value.AdvertiseHost == "" {
			addresses, err := net.InterfaceAddrs()
			if err != nil {
				return value, err
			}
			for _, address := range addresses {
				if ip, ok := address.(*net.IPNet); ok && ip.IP.To4() != nil && !ip.IP.IsLoopback() && !ip.IP.IsUnspecified() {
					if value.AdvertiseHost != "" {
						return value, errors.New("bridge has ambiguous task network addresses")
					}
					value.AdvertiseHost = ip.IP.String()
				}
			}
			if value.AdvertiseHost == "" {
				return value, errors.New("bridge task address unavailable")
			}
		}
		return value, nil
	}
	return managed.Serve(ctx, managed.ServeOptions{Config: c, Backend: backend, NewNative: func(keys *sshcredentials.Credentials) (managed.NativeServer, error) {
		var dialect sshbridge.Dialect
		switch c.BridgeType {
		case "hermes-ssh":
			dialect = hermes.Dialect{}
		case "openclaw-ssh":
			dialect = openclaw.Dialect{}
		default:
			return nil, errors.New("unsupported bridge protocol")
		}
		return sshbridge.New(sshbridge.Options{Dialect: dialect, Credentials: keys, ResolveListen: listen, OutputDir: c.OutputDir, Logger: logger, OmitRawLog: !c.RetainRawLog})
	}})
}

var _ managed.NativeServer = (*sshbridge.Manager)(nil)
