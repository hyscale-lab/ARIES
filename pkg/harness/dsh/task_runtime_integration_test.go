//go:build integration

package dsh

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
)

// Execute against the native runtime and adapter shipped in the prepared image.
// Node's test runner also detects uncaught child errors and rejected promises.
func TestDSHDrainerStartupAndReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	versions, err := config.LoadVersions(filepath.Join("../../..", "configs/versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	image, err := LocalImage(versions.DSH.Image, versions.DSH.Version)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := dockerdeployment.New(dockerdeployment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	id, createErr := provider.Create(ctx, deployment.Request{
		Image: image, Init: true, Entrypoint: []string{"/bin/sleep", "infinity"},
		Placement: core.RuntimePlacement{AttachmentID: "none"},
		Labels:    map[string]string{"aries.test": "dsh-drainer-startup"},
	})
	if id != "" {
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
			defer done()
			if err := provider.Stop(cleanup, id); err != nil {
				t.Error(err)
			}
		})
	}
	if createErr != nil {
		t.Fatal(createErr)
	}
	if err := provider.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("testdata/task-runtime.test.mjs")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := harnesscommon.StageArchive(nil, map[string]harnesscommon.ArchiveFile{
		"task-runtime.test.mjs": {Content: script, Mode: 0600},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(archive)
	if err := provider.UploadArchive(ctx, id, "/tmp", bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	result, err := provider.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c",
		"mkdir /tmp/aries-dsh-runtime && tar -xzf /opt/aries/dsh-helper.tar.gz -C /tmp/aries-dsh-runtime && cp /tmp/task-runtime.test.mjs /tmp/aries-dsh-runtime/node_modules/@deepseek-ai/dsh-ssh/lib/ && /tmp/aries-dsh-runtime/node --test /tmp/aries-dsh-runtime/node_modules/@deepseek-ai/dsh-ssh/lib/task-runtime.test.mjs",
	}})
	t.Log(result.Stdout)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("native runtime regressions: exit=%d, error=%v\n%s", result.ExitCode, err, result.Stderr)
	}
}
