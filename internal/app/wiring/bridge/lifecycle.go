package bridge

import (
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/bridge/lifecycle"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// own composes an access adapter with the occurrence's tool sandbox, which the
// returned bridge then creates, suspends, and removes.
func own(cfg config.Config, outputRoot string, sandbox app.SandboxInstance, access lifecycle.Access, logger *logrus.Logger) (runner.ToolBridge, error) {
	mode, err := lifecycle.ParseMode(cfg.Bridge.SandboxLifecycle)
	if err != nil {
		return nil, fmt.Errorf("bridge.sandbox_lifecycle: %w", err)
	}
	bridge, err := lifecycle.New(lifecycle.Options{ToolSandbox: sandbox.Sandbox, Access: access, Mode: mode, OutputDir: outputRoot, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("construct bridge sandbox lifecycle: %w", err)
	}
	return bridge, nil
}
