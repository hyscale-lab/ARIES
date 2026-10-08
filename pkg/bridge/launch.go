package bridge

import (
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// LaunchSpec is supplied by composition wiring. Request describes bridge runtime
// placement; Config describes the child and its independent sandbox backend.
// Request.Workdir is the private bootstrap directory containing config.json.
// The manager adds occurrence identity, ownership labels and task attachment.
type LaunchSpec struct {
	Request deployment.Request
	Config  LaunchConfig
	// RuntimeBackend and ResourceMetrics describe the bridge runtime, not the
	// borrowed sandbox. Use "unsupported" when resource measurement is unavailable.
	RuntimeBackend  string
	ResourceMetrics string
}

// LaunchConfig describes a single-use bridge runtime. Secrets reside in fixed,
// privately staged files beside this configuration, never argv or environment.
type LaunchConfig struct {
	InstanceID     string            `json:"instance_id"`
	BridgeType     string            `json:"bridge_type"`
	Backend        string            `json:"backend"` // Sandbox execution backend, not bridge runtime placement.
	DockerSocket   string            `json:"docker_socket,omitempty"`
	Listen         core.BridgeListen `json:"listen"`
	ControlAddress string            `json:"control_address"`
	OutputDir      string            `json:"output_dir"`
	RetainRawLog   bool              `json:"retain_raw_log"`
}
