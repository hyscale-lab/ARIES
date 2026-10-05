package docker

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

const (
	testBase       = "docker.io/example/base:v1"
	testImage      = "aries-local/example:v1-derived"
	testDockerfile = "ARG BASE\nFROM ${BASE}\nRUN true\n"
)

type fakeImageBuilder struct {
	present    bool
	inspectErr error
	stream     string
	builds     []client.ImageBuildOptions
	dockerfile string
}

func (fake *fakeImageBuilder) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if fake.inspectErr != nil {
		return client.ImageInspectResult{}, fake.inspectErr
	}
	if !fake.present {
		return client.ImageInspectResult{}, errdefs.ErrNotFound
	}
	return client.ImageInspectResult{}, nil
}

func (fake *fakeImageBuilder) ImageBuild(_ context.Context, buildContext io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error) {
	fake.builds = append(fake.builds, options)
	archive := tar.NewReader(buildContext)
	header, err := archive.Next()
	if err != nil || header.Name != "Dockerfile" {
		return client.ImageBuildResult{}, errors.New("build context holds no Dockerfile")
	}
	content, _ := io.ReadAll(archive)
	fake.dockerfile = string(content)
	if !strings.Contains(fake.stream, "error") {
		fake.present = true
	}
	return client.ImageBuildResult{Body: io.NopCloser(strings.NewReader(fake.stream))}, nil
}

func TestBuildImageReusesCachedImage(t *testing.T) {
	fake := &fakeImageBuilder{present: true}
	if err := buildImage(context.Background(), fake, testImage, testDockerfile, map[string]string{"BASE": testBase}); err != nil {
		t.Fatal(err)
	}
	if len(fake.builds) != 0 {
		t.Fatalf("cached image was rebuilt: %d builds", len(fake.builds))
	}
}

func TestBuildImageBuildsMissingImageFromBase(t *testing.T) {
	fake := &fakeImageBuilder{stream: `{"stream":"Step 1/3"}` + "\n" + `{"stream":"Successfully built"}` + "\n"}
	if err := buildImage(context.Background(), fake, testImage, testDockerfile, map[string]string{"BASE": testBase}); err != nil {
		t.Fatal(err)
	}
	if len(fake.builds) != 1 {
		t.Fatalf("builds = %d", len(fake.builds))
	}
	build := fake.builds[0]
	if len(build.Tags) != 1 || build.Tags[0] != testImage || build.BuildArgs["BASE"] == nil || *build.BuildArgs["BASE"] != testBase {
		t.Fatalf("build options = %#v", build)
	}
	if fake.dockerfile != testDockerfile {
		t.Fatalf("Dockerfile = %q", fake.dockerfile)
	}
}

func TestBuildImageReportsBuildFailures(t *testing.T) {
	failed := &fakeImageBuilder{stream: `{"stream":"Step 1/3"}` + "\n" + `{"errorDetail":{"message":"uv: network unreachable"},"error":"uv: network unreachable"}` + "\n"}
	if err := buildImage(context.Background(), failed, testImage, testDockerfile, nil); err == nil || !strings.Contains(err.Error(), "network unreachable") {
		t.Fatalf("stream error = %v", err)
	}
	broken := &fakeImageBuilder{inspectErr: errors.New("daemon unavailable")}
	if err := buildImage(context.Background(), broken, testImage, testDockerfile, nil); err == nil || len(broken.builds) != 0 {
		t.Fatalf("inspect error = %v, builds = %d", err, len(broken.builds))
	}
}
