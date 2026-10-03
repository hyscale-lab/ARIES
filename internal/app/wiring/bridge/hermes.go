package bridge

import (
	"context"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// NewHermes constructs the embedded Hermes SSH bridge.
func NewHermes(cfg config.Config, outputRoot string, resolveListen func(context.Context) (core.BridgeListen, error), logger *logrus.Logger) (runner.ToolBridge, error) {
	// Hermes runs OpenSSH itself, so this bridge stages no client helper
	// and needs no path to the ARIES executable.
	bridge, err := hermesssh.New(hermesssh.Options{ResolveListen: resolveListen, OutputDir: outputRoot, Logger: logger, OmitRawLog: !cfg.Bridge.RetainBridgeRawLog()})
	if err != nil {
		return nil, fmt.Errorf("construct Hermes SSH bridge: %w", err)
	}
	return bridge, nil
}
