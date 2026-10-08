package bridge

import (
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// LaunchSpec separates bridge placement from the borrowed sandbox backend.
// The service supplies run identity and labels; Request.Workdir holds config.json.
type LaunchSpec struct {
	Request         deployment.Request
	Config          LaunchConfig
	RuntimeBackend  string
	ResourceMetrics string
}

// LaunchConfig describes the run-owned bridge. Its host key is generated in memory.
type LaunchConfig struct {
	RunID           string            `json:"run_id"`
	BridgeType      string            `json:"bridge_type"`
	Backend         string            `json:"backend"`
	BackendEndpoint string            `json:"backend_endpoint,omitempty"`
	Listen          core.BridgeListen `json:"listen"`
	ControlAddress  string            `json:"control_address"`
	OutputDir       string            `json:"output_dir"`
	RetainRawLog    bool              `json:"retain_raw_log"`
}
