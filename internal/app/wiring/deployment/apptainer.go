package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment/apptainer"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/sirupsen/logrus"
)

// apptainerBridgeEntrypoint is the bridge image's program, which an Apptainer
// run starts from the aries-bridge built beside aries.
const apptainerBridgeEntrypoint = "/usr/local/bin/aries-bridge"

func NewApptainer(cfg config.DeploymentConfig, logger *logrus.Logger) (*apptainer.Manager, error) {
	if cfg.Backend != "apptainer" || cfg.Apptainer == nil {
		return nil, fmt.Errorf("deployment backend %q is not apptainer", cfg.Backend)
	}
	a := cfg.Apptainer
	return apptainer.New(apptainer.Options{
		ApptainerPath: a.Binary, StateDir: a.StateDir, ImageDir: a.ImageDir, OverlayDir: a.OverlayDir,
		UnenforcedLimits: a.UnenforcedLimits, DNS: apptainer.RunNamespaceDNS(), Logger: logger,
	})
}

// NewApptainerSandbox constructs the transport and the resource source for one
// occurrence's instances, measured from /proc.
func NewApptainerSandbox(cfg config.DeploymentConfig, runID, occurrenceID string, logger *logrus.Logger) (*apptainer.Manager, monitor.ResourceSource, error) {
	transport, err := NewApptainer(cfg, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("construct sandbox deployment: %w", err)
	}
	source, err := apptainer.NewResourceSource(apptainer.ResourceOptions{StateDir: cfg.Apptainer.StateDir, RunID: runID, TaskIDs: []string{occurrenceID}})
	if err != nil {
		return nil, nil, fmt.Errorf("construct sandbox resources: %w", err)
	}
	return transport, source, nil
}

// PullApptainerImages checks the Apptainer version and unpacks each image,
// from the SIF that deployment.apptainer.images maps it to, if any, or from
// its registry.
func PullApptainerImages(ctx context.Context, cfg config.DeploymentConfig, images []string) error {
	if cfg.Apptainer == nil {
		return errors.New("Apptainer image preparation requires deployment.apptainer")
	}
	if err := apptainer.CheckVersion(ctx, cfg.Apptainer.Binary); err != nil {
		return err
	}
	return apptainer.PullImages(ctx, cfg.Apptainer.Binary, cfg.Apptainer.ImageDir, images, cfg.Apptainer.Images)
}

// PrepareApptainerImage unpacks an image ARIES would build with Docker, such
// as Hermes with its telemetry plugin, from the SIF it is mapped to.
func PrepareApptainerImage(ctx context.Context, cfg config.DeploymentConfig, image string) error {
	source := ""
	if cfg.Apptainer != nil {
		source = cfg.Apptainer.Images[image]
	}
	if source == "" {
		return fmt.Errorf("Apptainer cannot build %s; build it on a Docker host (aries setup there), save it with docker save, convert it with apptainer build NAME.sif docker-archive://ARCHIVE.tar, and map %s to the SIF in deployment.apptainer.images", image, image)
	}
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("Apptainer image %s maps to %s: %w", image, source, err)
	}
	return apptainer.PullImages(ctx, cfg.Apptainer.Binary, cfg.Apptainer.ImageDir, []string{image}, cfg.Apptainer.Images)
}

// ApptainerBridge returns the bridge's runtime, a plain process beside the
// instances, with the workdir its launch must use and the sandbox execution
// endpoint, the state directory where the bridge reads sandbox records.
func ApptainerBridge(cfg config.DeploymentConfig, runID string) (*apptainer.ProcessRuntime, string, string, error) {
	if cfg.Apptainer == nil {
		return nil, "", "", errors.New("Apptainer bridge requires deployment.apptainer")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, "", "", err
	}
	program := filepath.Join(filepath.Dir(executable), "aries-bridge")
	runtime, err := apptainer.NewProcessRuntime(map[string]string{apptainerBridgeEntrypoint: program})
	if err != nil {
		return nil, "", "", err
	}
	workdir := filepath.Join(cfg.Apptainer.StateDir, "bridge-"+runID)
	return runtime, workdir, cfg.Apptainer.StateDir, nil
}

// PrepareApptainerBridge confirms the bridge program a run will start exists.
func PrepareApptainerBridge() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	program := filepath.Join(filepath.Dir(executable), "aries-bridge")
	if info, err := os.Stat(program); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("an Apptainer run starts the bridge from %s; build it with make build", program)
	}
	return nil
}

// ApptainerNetworkPolicy is recorded as an Apptainer run's network policy.
const ApptainerNetworkPolicy = apptainer.NetworkPolicy

// NewApptainerRunResources observes the run's bridge process.
func NewApptainerRunResources(cfg config.DeploymentConfig, runID string) (monitor.ResourceSource, error) {
	if cfg.Apptainer == nil {
		return nil, errors.New("bridge.deployment.apptainer is required")
	}
	return apptainer.NewResourceSource(apptainer.ResourceOptions{StateDir: cfg.Apptainer.StateDir, Scope: "run", RunID: runID})
}

// WaitForApptainerRunNamespace holds a run that RunApptainerInNamespace started
// until its namespace has internet access; anything else returns false.
func WaitForApptainerRunNamespace() (bool, error) { return apptainer.WaitForRunNamespace() }

// RunApptainerInNamespace re-executes argv in the run's private network
// namespace and returns its exit status.
func RunApptainerInNamespace(ctx context.Context, argv []string) (int, error) {
	return apptainer.RunInNamespace(ctx, argv, "")
}
