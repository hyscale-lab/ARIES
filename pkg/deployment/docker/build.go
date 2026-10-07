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
	host := socket
	if host == "" {
		host = defaultDockerSocket
	}
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-setup/1"))
	if err != nil {
		return fmt.Errorf("create Docker client: %w", err)
	}
	return errors.Join(buildImage(ctx, api, image, dockerfile, buildArgs), api.Close())
}

func buildImage(ctx context.Context, api imageBuilder, image, dockerfile string, buildArgs map[string]string) error {
	if err := containerimage.ValidatePinnedTagOnly(image); err != nil {
		return fmt.Errorf("build Docker image %q: %w", image, err)
	}
	if _, err := api.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect Docker image %q: %w", image, err)
	}
	buildContext, err := dockerfileArchive(dockerfile)
	if err != nil {
		return err
	}
	args := make(map[string]*string, len(buildArgs))
	for name, value := range buildArgs {
		args[name] = &value
	}
	logger := logrus.WithContext(ctx).WithField("image", image)
	logger.Info("building Docker image")
	started := time.Now()
	result, err := api.ImageBuild(ctx, buildContext, client.ImageBuildOptions{
		Tags: []string{image}, Dockerfile: "Dockerfile", BuildArgs: args, Remove: true, ForceRemove: true,
	})
	if err != nil {
		return fmt.Errorf("build Docker image %q: %w", image, err)
	}
	streamErr := buildStreamError(result.Body)
	closeErr := result.Body.Close()
	if streamErr != nil || closeErr != nil {
		return fmt.Errorf("build Docker image %q: %w", image, errors.Join(streamErr, closeErr))
	}
	if _, err := api.ImageInspect(ctx, image); err != nil {
		return fmt.Errorf("confirm Docker image %q after build: %w", image, err)
	}
	logger.WithField("duration", time.Since(started).Round(time.Second).String()).Info("built Docker image")
	return nil
}

// dockerfileArchive is a build context holding only the Dockerfile.
func dockerfileArchive(content string) (io.Reader, error) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(content))}); err != nil {
		return nil, fmt.Errorf("write Docker build context: %w", err)
	}
	if _, err := writer.Write([]byte(content)); err != nil {
		return nil, fmt.Errorf("write Docker build context: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("write Docker build context: %w", err)
	}
	return &archive, nil
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
