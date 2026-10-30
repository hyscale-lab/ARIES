package bridge

import (
	"errors"
	"os"
	"path/filepath"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/codex"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// NewService takes ownership of the supplied run-scoped runtime transport.
func NewService(cfg config.Config, runID, outputRoot string, placement core.RuntimePlacement, runtime deployment.Runtime, launch managed.LaunchSpec) (*managed.Service, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	helper := "aries-ssh-client"
	if cfg.Bridge.Type == "codex-ssh" {
		helper = "aries-codex-ssh"
	}
	client, err := SSHClientConfig(cfg.Bridge.Type, filepath.Join(filepath.Dir(executable), helper))
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	staging, err := SandboxStaging(cfg)
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	service, err := managed.NewService(managed.Options{Runtime: runtime, Launch: launch, Client: client, Staging: staging, RunID: runID, Placement: placement, OutputDir: outputRoot, BridgeType: cfg.Bridge.Type, RetainRawLog: cfg.Bridge.RetainBridgeRawLog()})
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	return service, nil
}

// SandboxStaging selects what the session places into the task sandbox. Only
// Codex needs an executor that its task images do not carry.
func SandboxStaging(cfg config.Config) (managed.SandboxStaging, error) {
	switch cfg.Bridge.Type {
	case "codex-ssh":
		if cfg.Harness.Codex == nil {
			return nil, errors.New("Codex bridge requires harness.codex")
		}
		return codex.NewStaging(cfg.Harness.Codex.ResolvedExecutable)
	default:
		return nil, nil
	}
}
