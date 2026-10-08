package bridge

import (
	"errors"
	"os"
	"path/filepath"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
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
	client, err := SSHClientConfig(cfg.Bridge.Type, filepath.Join(filepath.Dir(executable), "aries-ssh-client"))
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	service, err := managed.NewService(managed.Options{Runtime: runtime, Launch: launch, Client: client, RunID: runID, Placement: placement, OutputDir: outputRoot, BridgeType: cfg.Bridge.Type, RetainRawLog: cfg.Bridge.RetainBridgeRawLog()})
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	return service, nil
}
