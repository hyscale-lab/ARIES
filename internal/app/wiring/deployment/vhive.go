package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

func vhiveOptions(cfg config.DeploymentConfig, logger *logrus.Logger) vhive.Options {
	v := cfg.VHive
	return vhive.Options{Socket: v.Socket, Namespace: v.Namespace, Snapshotter: v.Snapshotter, IODir: v.IODir, IdleMode: v.IdleMode, CheckpointDir: v.CheckpointDir, ShimDir: v.ShimDir, CNINetwork: v.CNINetwork, CNIBinPaths: v.CNIBinPaths, CNIConfDir: v.CNIConfDir, CNICacheDir: v.CNICacheDir, VCPUs: v.VCPUs, MemoryMB: v.MemoryMB, Logger: logger}
}

func NewVHiveSandbox(cfg config.Config, logger *logrus.Logger) (*vhive.Manager, monitor.ResourceSource, error) {
	options := vhiveOptions(cfg.Sandbox.Deployment, logger)
	// Benchmarks prepare images in Docker, including locally built task images.
	// Export through Moby, then import through containerd's native image API.
	options.ImageArchive = func(ctx context.Context, image string) (io.ReadCloser, error) {
		api, err := client.New(client.WithHost("unix://" + cfg.Harness.Deployment.Docker.Socket))
		if err != nil {
			return nil, err
		}
		archive, err := api.ImageSave(ctx, []string{image})
		if err != nil {
			return nil, errors.Join(err, api.Close())
		}
		return &dockerImageArchive{ReadCloser: archive, closeClient: api.Close}, nil
	}
	m, err := vhive.New(options)
	if err != nil {
		return nil, nil, err
	}
	return m, vhiveResources{}, nil
}

type dockerImageArchive struct {
	io.ReadCloser
	closeClient func() error
}

func (a *dockerImageArchive) Close() error { return errors.Join(a.ReadCloser.Close(), a.closeClient()) }

type vhiveResources struct{}

func (vhiveResources) Sample(context.Context) ([]core.ResourceReading, error) {
	return nil, fmt.Errorf("%w: vHive VM resource observation is not implemented", monitor.ErrUnsupported)
}
func (vhiveResources) Close() error { return nil }

// VHiveExecutionAccess preserves the host FIFO paths: containerd's guest IO
// proxy opens exactly the paths supplied by the independent bridge process.
func VHiveExecutionAccess(cfg config.DeploymentConfig) (string, []deployment.Mount, error) {
	o := vhiveOptions(cfg, nil)
	data, err := json.Marshal(vhive.ExecutionOptions{Socket: o.Socket, Namespace: o.Namespace, IODir: o.IODir, IdleMode: o.IdleMode, CheckpointDir: o.CheckpointDir, ShimDir: o.ShimDir})
	if err != nil {
		return "", nil, err
	}
	if err = os.MkdirAll(o.IODir, 0o700); err != nil {
		return "", nil, err
	}
	mounts := []deployment.Mount{{Source: o.Socket, Target: o.Socket}, {Source: o.IODir, Target: o.IODir}}
	if o.IdleMode == "checkpoint" {
		info, err := os.Stat(o.ShimDir)
		if err != nil {
			return "", nil, fmt.Errorf("inspect vHive shim_dir: %w", err)
		}
		if !info.IsDir() {
			return "", nil, errors.New("vHive shim_dir must be an existing directory")
		}
		if err := os.MkdirAll(o.CheckpointDir, 0o700); err != nil {
			return "", nil, err
		}
		for _, path := range []string{o.Socket + ".ttrpc", o.CheckpointDir, o.ShimDir} {
			mounts = append(mounts, deployment.Mount{Source: path, Target: path})
		}
	}
	return string(data), mounts, nil
}

// VHiveTaskEnvironment rejects service address contracts the VM backend cannot
// honor. The logical handle continues borrowing the run's Docker connectivity.
func VHiveTaskEnvironment(base deployment.TaskEnvironment) deployment.TaskEnvironment {
	return vhiveTaskEnvironment{base}
}

type vhiveTaskEnvironment struct{ deployment.TaskEnvironment }

func (e vhiveTaskEnvironment) Start(ctx context.Context, r deployment.TaskEnvironmentRequest) (core.HarnessConnectivity, error) {
	if r.Environment.Services.SearchPort != 0 || len(r.Environment.Services.MCP) != 0 {
		return core.HarnessConnectivity{}, errors.New("vHive does not yet support benchmark services inside the sandbox")
	}
	return e.TaskEnvironment.Start(ctx, r)
}
