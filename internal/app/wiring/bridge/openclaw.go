package bridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// NewOpenClaw constructs the embedded OpenClaw SSH bridge.
func NewOpenClaw(cfg config.Config, outputRoot string, resolveListen func(context.Context) (core.BridgeListen, error), logger *logrus.Logger) (runner.ToolBridge, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate ARIES executable: %w", err)
	}
	bridge, err := openclawssh.New(openclawssh.Options{ResolveListen: resolveListen, OutputDir: outputRoot, ClientPath: filepath.Join(filepath.Dir(executable), "aries-ssh"), Logger: logger, OmitRawLog: !cfg.Bridge.RetainBridgeRawLog()})
	if err != nil {
		return nil, fmt.Errorf("construct OpenClaw SSH bridge: %w", err)
	}
	return bridge, nil
}
