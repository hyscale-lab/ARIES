package bridge

import (
	"errors"
	"os"
	"path/filepath"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// New takes ownership of the supplied runtime and constructs its bridge lifecycle.
// Provider selection and execution access are supplied by composition.
func New(cfg config.Config, outputRoot string, runtime deployment.Runtime, launch managed.LaunchSpec) (runner.ToolBridge, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	client, err := SSHClientConfig(cfg.Bridge.Type, filepath.Join(filepath.Dir(executable), "aries-ssh-client"))
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	manager, err := managed.New(managed.Options{Runtime: runtime, Launch: launch, Client: client, OutputDir: outputRoot, BridgeType: cfg.Bridge.Type, RetainRawLog: cfg.Bridge.RetainBridgeRawLog()})
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	return manager, nil
}
