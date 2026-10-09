package bridge

import (
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// NewHermes constructs the embedded Hermes SSH bridge, which owns the
// occurrence's tool sandbox.
func NewHermes(cfg config.Config, outputRoot string, sandbox app.SandboxInstance, logger *logrus.Logger) (runner.ToolBridge, error) {
	// Hermes runs OpenSSH itself, so this bridge stages no client helper
	// and needs no path to the ARIES executable.
	bridge, err := hermesssh.New(hermesssh.Options{ResolveListen: sandbox.BridgeListen, OutputDir: outputRoot, Logger: logger, OmitRawLog: !cfg.Bridge.RetainBridgeRawLog()})
	if err != nil {
		return nil, fmt.Errorf("construct Hermes SSH bridge: %w", err)
	}
	return own(cfg, outputRoot, sandbox, bridge, logger)
}
