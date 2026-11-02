package harness

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/agentplugin"
	"github.com/sirupsen/logrus"
)

// pluginCacheRoot holds checkouts of pinned plugin repositories, relative to
// the working directory like the benchmark checkouts.
const pluginCacheRoot = ".cache/plugins"

// loaded holds each package as this process first read it. A run prepares its
// plugins before its first task, right after verifying each checkout, and
// every task then stages those bytes rather than reading the cache again.
// One aries process runs one profile.
var loaded = struct {
	sync.Mutex
	plugins map[string]harnesscommon.Plugin
}{plugins: map[string]harnesscommon.Plugin{}}

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

// LoadPlugins returns the profile's selected plugin packages, prepared by
// PreparePlugins, and applies each entry's secret_env to the package's
// command MCP servers. Each package is read once per process; later calls
// return copies of what was read.
func LoadPlugins(cfg config.Config) ([]harnesscommon.Plugin, error) {
	plugins := make([]harnesscommon.Plugin, 0, len(cfg.Harness.Plugins))
	for _, selected := range cfg.Harness.Plugins {
		pin, ok := cfg.Versions.Plugins[selected.Name]
		if !ok {
			return nil, fmt.Errorf("plugin %q is not pinned in the version catalog", selected.Name)
		}
		root, source, checkout := pin.ResolvedPath, "path "+pin.ResolvedPath, ""
		if pin.RepositoryURL != "" {
			root = checkoutRoot(selected.Name, pin)
			source = pin.RepositoryURL + "@" + pin.Revision
			if pin.Subdirectory != "" {
				checkout, root = root, filepath.Join(root, pin.Subdirectory)
				source += " " + pin.Subdirectory
			}
		}
		plugin, err := loadOnce(root, checkout, selected.Name, source)
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

// insideCheckout resolves a pinned subdirectory and refuses one that a
// symbolic link takes out of its checkout: its files would be recorded as the
// pinned commit's.
func insideCheckout(checkout, subdirectory string) (string, error) {
	base, err := realPath(checkout)
	if err != nil {
		return "", fmt.Errorf("resolve checkout: %w", err)
	}
	resolved, err := realPath(subdirectory)
	if err != nil {
		return "", fmt.Errorf("resolve subdirectory: %w", err)
	}
	relative, err := filepath.Rel(base, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("subdirectory leaves the checkout: it resolves to %s", resolved)
	}
	return resolved, nil
}

// realPath is path made absolute, with every symbolic link resolved.
func realPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// loadOnce reads a package the first time this process asks for it and
// returns a copy of that read every time. The key is the configured location,
// not where it resolves, and a pinned subdirectory is checked against its
// checkout (when checkout is set) on the first read only: a link changed in
// the cache during the run cannot make a later task stage another package.
func loadOnce(root, checkout, name, source string) (harnesscommon.Plugin, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return harnesscommon.Plugin{}, fmt.Errorf("plugin %q: %w", name, err)
	}
	key := name + "\x00" + source + "\x00" + absolute
	loaded.Lock()
	defer loaded.Unlock()
	if plugin, ok := loaded.plugins[key]; ok {
		return clonePlugin(plugin), nil
	}
	read := absolute
	if checkout != "" {
		if read, err = insideCheckout(checkout, absolute); err != nil {
			return harnesscommon.Plugin{}, fmt.Errorf("plugin %q: %w", name, err)
		}
	}
	plugin, err := agentplugin.Load(read, name, source)
	if err != nil {
		return harnesscommon.Plugin{}, err
	}
	loaded.plugins[key] = plugin
	return clonePlugin(plugin), nil
}

// clonePlugin copies everything a task's wiring or manager could change;
// file contents are shared and only read.
func clonePlugin(plugin harnesscommon.Plugin) harnesscommon.Plugin {
	plugin.Files = slices.Clone(plugin.Files)
	plugin.Directories = slices.Clone(plugin.Directories)
	plugin.Skills = slices.Clone(plugin.Skills)
	plugin.Ignored = slices.Clone(plugin.Ignored)
	plugin.MCPServers = slices.Clone(plugin.MCPServers)
	for index := range plugin.MCPServers {
		server := &plugin.MCPServers[index]
		server.Args = slices.Clone(server.Args)
		server.Env, server.SecretEnv, server.Headers = maps.Clone(server.Env), maps.Clone(server.SecretEnv), maps.Clone(server.Headers)
	}
	return plugin
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

// reportIgnored logs what each package's manifest declared that ARIES
// ignored, which §5.2 and §8.1 have a client report. plugins.json records it
// too.
func reportIgnored(logger *logrus.Logger, plugins []harnesscommon.Plugin) {
	if logger == nil {
		return
	}
	for _, plugin := range plugins {
		for _, ignored := range plugin.Ignored {
			logger.WithField("plugin", plugin.Name).Warn("ignored " + ignored)
		}
	}
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
