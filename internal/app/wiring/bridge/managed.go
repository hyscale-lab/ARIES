package bridge

import (
	"errors"
	"os"
	"path/filepath"

	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

func newManaged(cfg config.Config, outputRoot string, logger *logrus.Logger) (runner.ToolBridge, error) {
	if err := cfg.NormalizeDeployment(); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	runtime, err := docker.New(docker.Options{Socket: cfg.Bridge.Deployment.Docker.Socket, Logger: logger})
	request := deployment.Request{Image: cfg.Versions.Bridge.Image}
	if err != nil {
		return nil, err
	}
	socket := ""
	if cfg.Sandbox.Deployment.Docker != nil {
		socket = cfg.Sandbox.Deployment.Docker.Socket
	}
	manager, err := managed.New(managed.Options{Runtime: runtime, Request: request, OutputDir: outputRoot, ClientPath: filepath.Join(filepath.Dir(executable), "aries-ssh"), DockerSocket: socket, BridgeType: cfg.Bridge.Type, RetainRawLog: cfg.Bridge.RetainBridgeRawLog()})
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	return manager, nil
}
