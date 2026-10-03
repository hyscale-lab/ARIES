package harness

import (
	"fmt"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

// ValidateMCPServers rejects invalid server configuration before deployment construction.
func ValidateMCPServers(cfg config.HarnessConfig) error {
	for _, server := range cfg.MCPServers {
		if err := core.ValidateMCPServer(server); err != nil {
			return fmt.Errorf("invalid mcp server config: %w", err)
		}
	}
	return nil
}
