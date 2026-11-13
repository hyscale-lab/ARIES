// k8s-image-pins prints the images k8s/publish-images.sh builds, from the
// version catalog, so the script and ARIES derive the same tags.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/config"
	hermesharness "github.com/hyscale-lab/aries/pkg/harness/hermes"
)

func main() {
	context := flag.String("hermes-context", "", "write the Hermes recipe and its build context to this directory")
	flag.Parse()
	if err := run(*context); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run prints one KEY=value line per pin. Values never contain a newline.
func run(context string) error {
	versions, err := config.LoadVersions("configs/versions.json")
	if err != nil {
		return err
	}
	pin := versions.Hermes.OTelPlugin
	plugin := hermesharness.OTelPlugin{RepositoryURL: pin.RepositoryURL, Version: pin.Version, Revision: pin.Revision}
	image, err := hermesharness.LocalImage(versions.Hermes.Image, plugin, versions.Hermes.GRPCIO)
	if err != nil {
		return err
	}
	args, err := hermesharness.ImageBuildArgs(versions.Hermes.Image, plugin, versions.Hermes.GRPCIO)
	if err != nil {
		return err
	}
	if context != "" {
		files := hermesharness.ImageContext()
		files["Dockerfile"] = []byte(hermesharness.ImageDockerfile)
		for name, content := range files {
			path := filepath.Join(context, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, content, 0o644); err != nil {
				return err
			}
		}
	}
	fmt.Printf("BRIDGE_IMAGE=%s\n", versions.Bridge.Image)
	fmt.Printf("BRIDGE_KUBERNETES_BASE_IMAGE=%s\n", versions.Bridge.KubernetesBaseImage)
	fmt.Printf("HERMES_IMAGE=%s\n", image)
	fmt.Printf("HERMES_BASE=%s\n", args["BASE"])
	fmt.Printf("HERMES_OTEL_PLUGIN=%s\n", args["OTEL_PLUGIN"])
	fmt.Printf("HERMES_GRPCIO=%s\n", args["GRPCIO"])
	return nil
}
