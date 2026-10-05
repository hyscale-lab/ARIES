package harness

import (
	"github.com/hyscale-lab/aries/pkg/config"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
)

// commonOptions copies shared profile inputs; native constructors own defaults.
func commonOptions(cfg config.HarnessConfig) harnesscommon.Options {
	return harnesscommon.Options{
		Mode:                   cfg.Mode,
		WebSearchEnabled:       cfg.WebSearch.Enabled,
		ExtractAPIKeyEnv:       cfg.WebSearch.ExtractAPIKeyEnv,
		SubagentsEnabled:       cfg.Subagents.Enabled != nil && *cfg.Subagents.Enabled,
		MaxConcurrentSubagents: cfg.Subagents.MaxConcurrent,
		MCPServers:             cfg.MCPServers,
	}
}
