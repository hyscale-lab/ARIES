package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
)

// Prepare builds the bridge server into the selected infrastructure image. It
// stages a closed build context and never accesses the configured model service.
func Prepare(ctx context.Context, cfg config.Config) error {
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
