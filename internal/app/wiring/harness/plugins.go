package harness

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/agentplugin"
)

// pluginCacheRoot holds checkouts of pinned plugin repositories, relative to
// the working directory like the benchmark checkouts.
const pluginCacheRoot = ".cache/plugins"

// PreparePlugins checks out every selected plugin pinned to a repository and
// validates every selected package, so a bad package fails during
// preparation rather than at a task's start. It contacts only the pinned
// repositories.
func PreparePlugins(ctx context.Context, cfg config.Config) error {
	for _, selected := range cfg.Harness.Plugins {
		pin := cfg.Versions.Plugins[selected.Name]
		if pin.RepositoryURL == "" {
			continue
		}
		if err := agentplugin.Fetch(ctx, checkoutRoot(selected.Name, pin), pin.RepositoryURL, pin.Revision); err != nil {
			return fmt.Errorf("prepare plugin %q: %w", selected.Name, err)
		}
	}
	_, err := LoadPlugins(cfg)
	return err
}

// LoadPlugins reads the profile's selected plugin packages, prepared by
// PreparePlugins, and applies each entry's secret_env to the package's
// command MCP servers.
func LoadPlugins(cfg config.Config) ([]harnesscommon.Plugin, error) {
	plugins := make([]harnesscommon.Plugin, 0, len(cfg.Harness.Plugins))
	for _, selected := range cfg.Harness.Plugins {
		pin, ok := cfg.Versions.Plugins[selected.Name]
		if !ok {
			return nil, fmt.Errorf("plugin %q is not pinned in the version catalog", selected.Name)
		}
		root, source := pin.ResolvedPath, "path "+pin.ResolvedPath
		if pin.RepositoryURL != "" {
			root = checkoutRoot(selected.Name, pin)
			source = pin.RepositoryURL + "@" + pin.Revision
			if pin.Subdirectory != "" {
				root = filepath.Join(root, pin.Subdirectory)
				source += " " + pin.Subdirectory
			}
		}
		plugin, err := agentplugin.Load(root, selected.Name, source)
		if err != nil {
			return nil, err
		}
		if len(selected.SecretEnv) > 0 {
			commands := 0
			for index := range plugin.MCPServers {
				server := &plugin.MCPServers[index]
				if server.Command == "" {
					continue
				}
				commands++
				server.SecretEnv = maps.Clone(selected.SecretEnv)
				if err := core.ValidateMCPServer(*server); err != nil {
					return nil, fmt.Errorf("plugin %q: %w", selected.Name, err)
				}
			}
			if commands == 0 {
				return nil, fmt.Errorf("plugin %q: secret_env needs a command MCP server in the package", selected.Name)
			}
		}
		plugins = append(plugins, plugin)
	}
	return plugins, nil
}

// withPluginServers appends the plugins' MCP servers to the profile's. A
// server name must be unique across both; task servers are checked against
// the result at Start (harness.MCPServers).
func withPluginServers(profile []core.MCPServerConfig, plugins []harnesscommon.Plugin) ([]core.MCPServerConfig, error) {
	owners := make(map[string]string, len(profile))
	for _, server := range profile {
		owners[server.Name] = "harness.mcp_servers"
	}
	servers := append([]core.MCPServerConfig(nil), profile...)
	for _, plugin := range plugins {
		for _, server := range plugin.MCPServers {
			if owner, taken := owners[server.Name]; taken {
				return nil, fmt.Errorf("plugin %q MCP server %q has the name of a server from %s", plugin.Name, server.Name, owner)
			}
			owners[server.Name] = "plugin " + plugin.Name
			servers = append(servers, server)
		}
	}
	return servers, nil
}

func checkoutRoot(name string, pin config.PluginVersions) string {
	return filepath.Join(pluginCacheRoot, name+"@"+pin.Revision)
}

// pluginOptions loads the profile's plugins into the shared harness options.
func pluginOptions(cfg config.Config, options *harnesscommon.Options) error {
	plugins, err := LoadPlugins(cfg)
	if err != nil {
		return err
	}
	servers, err := withPluginServers(options.MCPServers, plugins)
	if err != nil {
		return err
	}
	options.Plugins, options.MCPServers = plugins, servers
	return nil
}
