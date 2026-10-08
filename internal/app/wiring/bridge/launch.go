package bridge

import (
	"strings"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// DockerLaunch configures the supported Docker bridge / Docker sandbox pairing.
// The runtime provider interprets the socket mount and task placement; the shared
// bridge manager only adds occurrence identity and stages this configuration.
func DockerLaunch(image, sandboxSocket string) managed.LaunchSpec {
	socket := strings.TrimPrefix(sandboxSocket, "unix://")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	return managed.LaunchSpec{
		RuntimeBackend: "docker", ResourceMetrics: "docker-stats",
		Request: deployment.Request{
			Image: image, Workdir: "/tmp/aries-bridge",
			Entrypoint: []string{"/usr/local/bin/aries-bridge"}, Args: []string{"config.json"},
			ServicePort: 8443, HarnessPort: 2222, TrustedDockerSocket: socket,
		},
		Config: managed.LaunchConfig{
			Backend: "docker", DockerSocket: "/var/run/docker.sock",
			ControlAddress: "0.0.0.0:8443", OutputDir: "/tmp/aries-bridge/evidence",
			Listen: core.BridgeListen{BindHost: "0.0.0.0", BindPort: 2222, AdvertisePort: 2222},
		},
	}
}
