package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

type imageBuilder interface {
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImageBuild(context.Context, io.Reader, client.ImageBuildOptions) (client.ImageBuildResult, error)
}

// BuildImage builds image from dockerfile once and reuses it from the local
// Docker cache afterwards. The Dockerfile is the whole build context, and
// buildArgs fill its ARG values. Any base it names must already be present;
// network access during the build is the host's, never a task's.
func BuildImage(ctx context.Context, socket, image, dockerfile string, buildArgs map[string]string) error {
	api, err := newImageBuildClient(socket)
	if err != nil {
		return err
	}
	return errors.Join(buildImage(ctx, api, image, dockerfile, buildArgs), api.Close())
}

func newImageBuildClient(socket string) (*client.Client, error) {
	host := socket
	if host == "" {
		host = defaultDockerSocket
	}
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-setup/1"))
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return api, nil
}

func buildImage(ctx context.Context, api imageBuilder, image, dockerfile string, buildArgs map[string]string) error {
	archive, err := imageBuildArchive([]buildFile{{name: "Dockerfile", mode: 0o644, content: []byte(dockerfile)}})
	if err != nil {
		return err
	}
	return buildImageArchive(ctx, api, image, archive, buildArgs, nil)
}

// buildImageArchive reuses an existing image when all requested labels match.
// Without labels, the caller's image tag is the complete cache identity.
func buildImageArchive(ctx context.Context, api imageBuilder, image string, archive []byte, buildArgs, labels map[string]string) error {
	if err := containerimage.ValidatePinnedTagOnly(image); err != nil {
		return fmt.Errorf("build Docker image %q: %w", image, err)
	}
	existing, err := api.ImageInspect(ctx, image)
	if err == nil && imageBuildLabelsMatch(existing, labels) {
		return nil
	}
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect Docker image %q: %w", image, err)
	}
	args := make(map[string]*string, len(buildArgs))
	for name, value := range buildArgs {
		args[name] = &value
	}
	logger := logrus.WithContext(ctx).WithField("image", image)
	logger.Info("building Docker image")
	started := time.Now()
	result, err := api.ImageBuild(ctx, bytes.NewReader(archive), client.ImageBuildOptions{
		Tags: []string{image}, Dockerfile: "Dockerfile", BuildArgs: args, Labels: labels, Remove: true, ForceRemove: true,
	})
	if err != nil {
		return fmt.Errorf("build Docker image %q: %w", image, err)
	}
	streamErr := buildStreamError(result.Body)
	closeErr := result.Body.Close()
	if streamErr != nil || closeErr != nil {
		return fmt.Errorf("build Docker image %q: %w", image, errors.Join(streamErr, closeErr))
	}
	built, err := api.ImageInspect(ctx, image)
	if err != nil {
		return fmt.Errorf("confirm Docker image %q after build: %w", image, err)
	}
	if !imageBuildLabelsMatch(built, labels) {
		return fmt.Errorf("built Docker image %q does not match requested build labels", image)
	}
	logger.WithField("duration", time.Since(started).Round(time.Second).String()).Info("built Docker image")
	return nil
}

func imageBuildLabelsMatch(image client.ImageInspectResult, labels map[string]string) bool {
	for name, value := range labels {
		if image.Config == nil || image.Config.Labels[name] != value {
			return false
		}
	}
	return true
}

type buildFile struct {
	name    string
	mode    int64
	content []byte
}

// imageBuildArchive packages only the explicitly supplied files, in their given
// order, with fixed metadata so content fingerprints remain reproducible.
func imageBuildArchive(files []buildFile) ([]byte, error) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for _, file := range files {
		if err := writer.WriteHeader(&tar.Header{Name: file.name, Mode: file.mode, Size: int64(len(file.content)), Typeflag: tar.TypeReg}); err != nil {
			return nil, fmt.Errorf("write Docker build context: %w", err)
		}
		if _, err := writer.Write(file.content); err != nil {
			return nil, fmt.Errorf("write Docker build context: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("write Docker build context: %w", err)
	}
	return archive.Bytes(), nil
}

// buildStreamError reads the Engine's build progress to the end and returns
// the error it reports, which arrives in the stream rather than as an HTTP
// failure.
func buildStreamError(body io.Reader) error {
	decoder := json.NewDecoder(body)
	for {
		var message struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&message); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("read build output: %w", err)
		}
		if message.ErrorDetail.Message != "" {
			return errors.New(message.ErrorDetail.Message)
		}
		if message.Error != "" {
			return errors.New(message.Error)
		}
	}
}
