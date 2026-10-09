package bridge

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// NewOpenClaw constructs the embedded OpenClaw SSH bridge, which owns the
// occurrence's tool sandbox.
func NewOpenClaw(cfg config.Config, outputRoot string, sandbox app.SandboxInstance, logger *logrus.Logger) (runner.ToolBridge, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate ARIES executable: %w", err)
	}
	bridge, err := openclawssh.New(openclawssh.Options{ResolveListen: sandbox.BridgeListen, OutputDir: outputRoot, ClientPath: filepath.Join(filepath.Dir(executable), "aries-ssh"), Logger: logger, OmitRawLog: !cfg.Bridge.RetainBridgeRawLog()})
	if err != nil {
		return nil, fmt.Errorf("construct OpenClaw SSH bridge: %w", err)
	}
	return own(cfg, outputRoot, sandbox, bridge, logger)
}
