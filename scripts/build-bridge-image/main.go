// build-bridge-image packages the bridge server using catalog pins.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
)

func main() {
	socket := flag.String("socket", "/var/run/docker.sock", "local Docker socket")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := build(ctx, *socket); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func build(ctx context.Context, socket string) error {
	versions, err := config.LoadVersions("configs/versions.json")
	if err != nil {
		return err
	}
	recipe, err := os.ReadFile("Dockerfile.bridge")
	if err != nil {
		return err
	}
	pins := versions.Bridge
	return docker.BuildBridgeImage(ctx, socket, pins.Image, string(recipe), "bin/aries-bridge", map[string]string{
		"BASE_IMAGE": pins.BaseImage,
	})
}
