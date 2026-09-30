package harness

import (
	"github.com/hyscale-lab/aries/pkg/config"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
)

// commonOptions copies shared profile inputs; native constructors own defaults.
// RedactEnv names the benchmark's credentials, which the harness scrubs.
func commonOptions(cfg config.Config) harnesscommon.Options {
	return harnesscommon.Options{
		Mode:                   cfg.Harness.Mode,
		WebSearchEnabled:       cfg.Harness.WebSearch.Enabled,
		ExtractAPIKeyEnv:       cfg.Harness.WebSearch.ExtractAPIKeyEnv,
		SubagentsEnabled:       cfg.Harness.Subagents.Enabled != nil && *cfg.Harness.Subagents.Enabled,
		MaxConcurrentSubagents: cfg.Harness.Subagents.MaxConcurrent,
		MCPServers:             cfg.Harness.MCPServers,
		RedactEnv:              cfg.BenchmarkCredentialEnv(),
	}
}
