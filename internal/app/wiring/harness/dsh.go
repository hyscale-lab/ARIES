package harness

import (
	"context"
	"errors"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/dsh"
	"github.com/sirupsen/logrus"
)

// NewDSH takes ownership of transport, including closing it on construction failure.
func NewDSH(cfg config.Config, outputRoot string, lookup func(string) ([]byte, bool), logger *logrus.Logger, transport deployment.Deployment) (app.HarnessInstance, error) {
	image, err := dsh.LocalImage(cfg.Versions.DSH.Image, cfg.Versions.DSH.Version)
	if err != nil {
		return app.HarnessInstance{}, errors.Join(err, transport.Close())
	}
	manager, err := dsh.New(dsh.Options{
		Runtime: harnesscommon.RuntimeOptions{
			Deployment:   transport,
			Image:        image,
			OutputDir:    outputRoot,
			APIKeyLookup: lookup,
			Logger:       logger,
		},
		Common: commonOptions(cfg),
	})
	if err != nil {
		return app.HarnessInstance{}, errors.Join(err, transport.Close())
	}
	return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
}

func PrepareDSHImage(ctx context.Context, cfg config.Config, build func(context.Context, string, string, map[string]string) error) error {
	image, err := dsh.LocalImage(cfg.Versions.DSH.Image, cfg.Versions.DSH.Version)
	if err != nil {
		return err
	}
	return build(ctx, image, dsh.Dockerfile, map[string]string{"BASE": cfg.Versions.DSH.Image, "DSH_VERSION": cfg.Versions.DSH.Version})
}

// DSHBridgeImages supplies the native helper base and its separate bridge tag.
func DSHBridgeImages(cfg config.Config) (base, image string, err error) {
	base, err = dsh.LocalImage(cfg.Versions.DSH.Image, cfg.Versions.DSH.Version)
	if err != nil {
		return "", "", err
	}
	image, err = dsh.BridgeImage(cfg.Versions.DSH.Image, cfg.Versions.DSH.Version)
	return
}
