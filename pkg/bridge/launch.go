package bridge

import "github.com/hyscale-lab/aries/pkg/core"

// LaunchConfig describes a single-use bridge runtime. Secrets reside in fixed,
// privately staged files beside this configuration, never argv or environment.
type LaunchConfig struct {
	InstanceID     string            `json:"instance_id"`
	BridgeType     string            `json:"bridge_type"`
	Backend        string            `json:"backend"`
	DockerSocket   string            `json:"docker_socket,omitempty"`
	Listen         core.BridgeListen `json:"listen"`
	ControlAddress string            `json:"control_address"`
	OutputDir      string            `json:"output_dir"`
	RetainRawLog   bool              `json:"retain_raw_log"`
}
