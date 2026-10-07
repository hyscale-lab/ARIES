package bridge

import (
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// NewOpenClaw constructs a separately managed SSH bridge runtime.
func NewOpenClaw(cfg config.Config, outputRoot string, logger *logrus.Logger) (runner.ToolBridge, error) {
	return newManaged(cfg, outputRoot, logger)
}
