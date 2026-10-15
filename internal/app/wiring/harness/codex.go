package harness

import (
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
	codexharness "github.com/hyscale-lab/aries/pkg/harness/codex"
	"github.com/sirupsen/logrus"
)

// NewCodex takes ownership of transport, including closing it on construction failure.
func NewCodex(cfg config.Config, outputRoot string, lookup func(string) ([]byte, bool), logger *logrus.Logger, transport deployment.Deployment) (app.HarnessInstance, error) {
	if cfg.Harness.Codex == nil {
		return app.HarnessInstance{}, errors.Join(errors.New("construct Codex harness: harness.codex is required"), transport.Close())
	}
	manager, err := codexharness.New(codexharness.Options{
		Runtime: harnesscommon.RuntimeOptions{
			Deployment:   transport,
			Image:        cfg.Versions.Codex.Image,
			OutputDir:    outputRoot,
			APIKeyLookup: lookup,
			Logger:       logger,
		},
		CodexPath:    cfg.Harness.Codex.ResolvedExecutable,
		CodexVersion: cfg.Versions.Codex.Version,
	})
	if err != nil {
		return app.HarnessInstance{}, errors.Join(fmt.Errorf("construct Codex harness: %w", err), transport.Close())
	}
	return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
}
