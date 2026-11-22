package deployment

import (
	"context"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	kubernetesdeployment "github.com/hyscale-lab/aries/pkg/deployment/kubernetes"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/sirupsen/logrus"
)

func kubernetesOptions(cfg config.DeploymentConfig, logger *logrus.Logger) kubernetesdeployment.Options {
	k := cfg.Kubernetes
	return kubernetesdeployment.Options{
		Namespace: k.Namespace, NodeSelector: k.NodeSelector, PodCIDR: k.PodCIDR, ServiceCIDR: k.ServiceCIDR,
		ServiceAccount: k.ServiceAccount, ImagePullSecret: k.ImagePullSecret, Images: k.Images,
		RuntimeClass: k.RuntimeClass, AgentSandbox: k.AgentSandbox, WarmPool: k.WarmPool, Logger: logger,
	}
}

func NewKubernetes(cfg config.DeploymentConfig, logger *logrus.Logger) (*kubernetesdeployment.Manager, error) {
	return kubernetesdeployment.New(kubernetesOptions(cfg, logger))
}

// NewKubernetesHarness adds the staging roots a harness image needs. A pod's
// files are extracted by the image's own user, and OpenClaw runs as UID 1000
// while staging under root-owned /run and /opt. Hermes starts as root.
func NewKubernetesHarness(cfg config.DeploymentConfig, harnessType string, logger *logrus.Logger) (*kubernetesdeployment.Manager, error) {
	options := kubernetesOptions(cfg, logger)
	if harnessType == "openclaw" {
		options.StagingDirs = []string{"/run/aries", "/opt/aries"}
		options.StagingOwner = "1000:1000"
	}
	return kubernetesdeployment.New(options)
}

// NewKubernetesSandbox constructs the transport and occurrence-scoped monitoring
// source. The caller owns both on success.
func NewKubernetesSandbox(cfg config.DeploymentConfig, runID, occurrenceID string, logger *logrus.Logger) (*kubernetesdeployment.Manager, monitor.ResourceSource, error) {
	transport, err := NewKubernetes(cfg, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("construct sandbox deployment: %w", err)
	}
	source, err := newKubernetesResources(cfg, kubernetesdeployment.ResourceOptions{RunID: runID, TaskIDs: []string{occurrenceID}})
	if err != nil {
		return nil, nil, fmt.Errorf("construct Kubernetes resource source: %w", err)
	}
	return transport, source, nil
}

// NewKubernetesRunResources observes the run's shared bridge pod. It takes the
// sandbox placement, which names the Prometheus server for the whole run.
func NewKubernetesRunResources(sandbox config.DeploymentConfig, runID string) (monitor.ResourceSource, error) {
	return newKubernetesResources(sandbox, kubernetesdeployment.ResourceOptions{Scope: "run", RunID: runID})
}

// newKubernetesResources reads pod measurements from the configured Prometheus
// server. Without one the capability is absent, which is recorded as
// unsupported and lets the run continue.
func newKubernetesResources(sandbox config.DeploymentConfig, options kubernetesdeployment.ResourceOptions) (monitor.ResourceSource, error) {
	if sandbox.Kubernetes.PrometheusURL == "" {
		return unsupportedResources{}, nil
	}
	options.Namespace = sandbox.Kubernetes.Namespace
	options.PrometheusURL = sandbox.Kubernetes.PrometheusURL
	options.RuntimeClass = sandbox.Kubernetes.RuntimeClass
	return kubernetesdeployment.NewResourceSource(options)
}

type unsupportedResources struct{}

func (unsupportedResources) Sample(context.Context) ([]core.ResourceReading, error) {
	return nil, fmt.Errorf("%w: sandbox.deployment.kubernetes.prometheus_url is not set", monitor.ErrUnsupported)
}
func (unsupportedResources) Close() error { return nil }
