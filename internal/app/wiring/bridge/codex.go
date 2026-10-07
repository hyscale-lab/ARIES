package bridge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/bridge/codexssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// NewCodex constructs the embedded Codex SSH bridge with its staged helpers.
func NewCodex(cfg config.Config, outputRoot string, resolveListen func(context.Context) (core.BridgeListen, error), logger *logrus.Logger) (runner.ToolBridge, error) {
	if cfg.Harness.Codex == nil {
		return nil, errors.New("construct Codex SSH bridge: harness.codex is required")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate ARIES executable: %w", err)
	}
	binaryDir := filepath.Dir(executable)
	bridge, err := codexssh.New(codexssh.Options{
		ResolveListen: resolveListen,
		ClientPath:    filepath.Join(binaryDir, "aries-codex-ssh"), CodexPath: cfg.Harness.Codex.ResolvedExecutable,
		SupervisorPath: filepath.Join(binaryDir, "aries-codex-exec"), OutputDir: outputRoot, Logger: logger,
		OmitRawLog: !cfg.Bridge.RetainBridgeRawLog(),
	})
	if err != nil {
		return nil, fmt.Errorf("construct Codex SSH bridge: %w", err)
	}
	return bridge, nil
}
