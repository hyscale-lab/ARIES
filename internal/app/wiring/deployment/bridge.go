package deployment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
)

// DockerExecutionAccess supplies the endpoint visible to an independently
// deployed executor and the host attachment required to reach that endpoint.
func DockerExecutionAccess(socket string) (string, []deployment.Mount) {
	source := strings.TrimPrefix(socket, "unix://")
	if source == "" {
		source = "/var/run/docker.sock"
	}
	const endpoint = "/var/run/docker.sock"
	return endpoint, []deployment.Mount{{Source: source, Target: endpoint}}
}

// PrepareDockerBridge builds the bridge server into the selected infrastructure image. It
// stages a closed build context and never accesses the configured model service.
func PrepareDockerBridge(ctx context.Context, cfg config.Config) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	dockerfile, err := os.ReadFile("Dockerfile.bridge")
	if err != nil {
		return fmt.Errorf("read bridge image recipe: %w", err)
	}
	pins := cfg.Versions.Bridge
	return docker.BuildBridgeImage(ctx, cfg.Bridge.Deployment.Docker.Socket, pins.Image, string(dockerfile), filepath.Join(filepath.Dir(executable), "aries-bridge"), map[string]string{"BASE_IMAGE": pins.BaseImage})
}
