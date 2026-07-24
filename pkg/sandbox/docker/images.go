package docker

import (
	"context"
	"errors"
	"fmt"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

type imageClient interface {
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error)
}

// PullImages makes each configured image available in the local Docker Engine.
// Harness images remain digest pinned, while Terminal-Bench task images are
// explicit tags read from the pinned task checkout.
func PullImages(ctx context.Context, images []string) error {
	api, err := client.New(client.WithHost("unix://"+defaultDockerSocket), client.WithUserAgent("aries-setup/1"))
	if err != nil {
		return fmt.Errorf("create Docker client: %w", err)
	}
	return errors.Join(pullImages(ctx, api, images), api.Close())
}

func pullImages(ctx context.Context, api imageClient, images []string) error {
	seen := make(map[string]struct{}, len(images))
	for _, image := range images {
		if _, ok := seen[image]; ok {
			continue
		}
		seen[image] = struct{}{}
		if err := validatePullImage(image); err != nil {
			return fmt.Errorf("prepare Docker image %q: %w", image, err)
		}
		if _, err := api.ImageInspect(ctx, image); err == nil {
			continue
		} else if !cerrdefs.IsNotFound(err) {
			return fmt.Errorf("inspect Docker image %q: %w", image, err)
		}

		logrus.WithContext(ctx).WithField("image", image).Info("pulling Docker image")
		pull, err := api.ImagePull(ctx, image, client.ImagePullOptions{})
		if err != nil {
			return fmt.Errorf("pull Docker image %q: %w", image, err)
		}
		if err := pull.Wait(ctx); err != nil {
			_ = pull.Close()
			return fmt.Errorf("wait for Docker image pull %q: %w", image, err)
		}
		if err := pull.Close(); err != nil {
			return fmt.Errorf("close Docker image pull %q: %w", image, err)
		}
		if _, err := api.ImageInspect(ctx, image); err != nil {
			return fmt.Errorf("confirm Docker image %q after pull: %w", image, err)
		}
	}
	return nil
}

func validatePullImage(image string) error {
	if err := containerimage.Validate(image); err == nil {
		return nil
	}
	tagged, err := containerimage.ValidateTagOnly(image)
	if err != nil {
		return err
	}
	if tagged != image {
		return errors.New("image must not contain surrounding whitespace")
	}
	return nil
}
