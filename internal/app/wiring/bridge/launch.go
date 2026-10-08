package bridge

import (
	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// Launch describes the bridge process independently of its deployment provider.
// Composition supplies runtime metadata and sandbox execution access separately.
func Launch(image string) managed.LaunchSpec {
	return managed.LaunchSpec{
		Request: deployment.Request{
			Image: image, Workdir: "/tmp/aries-bridge",
			Entrypoint: []string{"/usr/local/bin/aries-bridge"}, Args: []string{"config.json"},
			ServicePort: 8443, InternalPort: 2222,
		},
		Config: managed.LaunchConfig{
			ControlAddress: "0.0.0.0:8443", OutputDir: "/tmp/aries-bridge/evidence",
			Listen: core.BridgeListen{BindHost: "0.0.0.0", BindPort: 2222, AdvertisePort: 2222},
		},
	}
}
