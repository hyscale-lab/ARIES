package bridge

import (
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// NewHermes constructs a separately managed SSH bridge runtime.
func NewHermes(cfg config.Config, outputRoot string, logger *logrus.Logger) (runner.ToolBridge, error) {
	return newManaged(cfg, outputRoot, logger)
}
