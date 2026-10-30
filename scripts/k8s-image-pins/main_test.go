package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	hermesharness "github.com/hyscale-lab/aries/pkg/harness/hermes"
)

// The published Hermes image comes from the same recipe, context and pins as
// the image aries setup builds, under the same tag.
func TestRunPublishesTheARIESHermesImageRecipe(t *testing.T) {
	t.Chdir(filepath.Join("..", ".."))
	dir := t.TempDir()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = write
	runErr := run(dir)
	os.Stdout = stdout
	write.Close()
	printed, _ := io.ReadAll(read)
	if runErr != nil {
		t.Fatal(runErr)
	}

	versions, err := config.LoadVersions("configs/versions.json")
	if err != nil {
		t.Fatal(err)
	}
	pin := versions.Hermes.OTelPlugin
	plugin := hermesharness.OTelPlugin{RepositoryURL: pin.RepositoryURL, Version: pin.Version, Revision: pin.Revision}
	image, err := hermesharness.LocalImage(versions.Hermes.Image, plugin, versions.Hermes.GRPCIO)
	if err != nil {
		t.Fatal(err)
	}
	args, err := hermesharness.ImageBuildArgs(versions.Hermes.Image, plugin, versions.Hermes.GRPCIO)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"HERMES_IMAGE=" + image, "HERMES_BASE=" + args["BASE"],
		"HERMES_OTEL_PLUGIN=" + args["OTEL_PLUGIN"], "HERMES_GRPCIO=" + args["GRPCIO"],
	} {
		if !strings.Contains(string(printed), want+"\n") {
			t.Fatalf("pins lack %q:\n%s", want, printed)
		}
	}

	want := hermesharness.ImageContext()
	want["Dockerfile"] = []byte(hermesharness.ImageDockerfile)
	var written []string
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, _ := filepath.Rel(dir, path)
		written = append(written, name)
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(content) != string(want[name]) {
			t.Errorf("%s differs from the recipe context", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != len(want) {
		t.Fatalf("context = %v", written)
	}
}
