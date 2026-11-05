package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

const (
	pluginsRoot    = "/run/aries/plugins"
	pluginDataRoot = "/run/aries/plugin-data"
	// commandCheckTimeout bounds the in-runtime check of one MCP command.
	commandCheckTimeout = 10 * time.Second
)

// Plugin is one portable plugin package (Agent Plugins v1) prepared for a
// harness runtime. Its files are staged under PluginRoot(Name); its skills are
// directories below PluginRoot(Name)/skills; its MCP servers already name
// runtime paths, with ${PLUGIN_ROOT} and ${PLUGIN_DATA} expanded. Native
// packages render the skills and servers in their own configuration formats.
type Plugin struct {
	Name       string
	Version    string
	Source     string
	TreeSHA256 string
	Files      []PluginFile
	Skills     []string
	MCPServers []core.MCPServerConfig
}

// PluginFile is one regular file of a plugin package. Path is slash-separated
// and relative to the plugin root.
type PluginFile struct {
	Path       string
	Content    []byte
	Executable bool
}

// PluginRoot is where a plugin's files are staged in the harness runtime.
func PluginRoot(name string) string { return pluginsRoot + "/" + name }

// PluginDataDir is a plugin's writable data directory in the harness runtime.
// It is created empty for every occurrence.
func PluginDataDir(name string) string { return pluginDataRoot + "/" + name }

// PluginSkillDirs returns the runtime skills directory of each plugin that
// has skills, in plugin order.
func PluginSkillDirs(plugins []Plugin) []string {
	var dirs []string
	for _, plugin := range plugins {
		if len(plugin.Skills) > 0 {
			dirs = append(dirs, PluginRoot(plugin.Name)+"/skills")
		}
	}
	return dirs
}

// PluginArchive returns the directories and files that stage plugins for a
// runtime user. Package files are read-only; each plugin's data directory is
// private and writable. Directories are listed parents first.
func PluginArchive(plugins []Plugin, uid, gid int) ([]ArchiveDirectory, map[string]ArchiveFile) {
	if len(plugins) == 0 {
		return nil, nil
	}
	root := strings.TrimPrefix(pluginsRoot, "/")
	dataRoot := strings.TrimPrefix(pluginDataRoot, "/")
	seen := map[string]bool{root: true, dataRoot: true}
	var names []string
	files := make(map[string]ArchiveFile)
	for _, plugin := range plugins {
		pluginDir := strings.TrimPrefix(PluginRoot(plugin.Name), "/")
		dataDir := strings.TrimPrefix(PluginDataDir(plugin.Name), "/")
		seen[pluginDir] = true
		seen[dataDir] = true
		for _, file := range plugin.Files {
			name := pluginDir + "/" + file.Path
			for dir := path.Dir(name); dir != pluginDir && !seen[dir]; dir = path.Dir(dir) {
				seen[dir] = true
			}
			mode := int64(0o444)
			if file.Executable {
				mode = 0o555
			}
			files[name] = ArchiveFile{Content: file.Content, Mode: mode, UID: uid, GID: gid}
		}
	}
	for dir := range seen {
		names = append(names, dir)
	}
	slices.Sort(names)
	directories := make([]ArchiveDirectory, 0, len(names))
	for _, dir := range names {
		mode := int64(0o755)
		if strings.HasPrefix(dir, dataRoot+"/") {
			mode = 0o700
		}
		directories = append(directories, ArchiveDirectory{Name: dir, Mode: mode, UID: uid, GID: gid})
	}
	return directories, files
}

// CheckPluginCommands confirms, inside the running runtime, that every plugin
// MCP command server can be launched: a bare command must resolve on the PATH
// the server itself gets and a staged one must be executable. The lookup uses
// the server's own environment and working directory, as the harness's launch
// does, so a package that puts its bin directory on PATH is found. Otherwise
// the run could proceed without the server's tools, with the cause only in a
// harness log.
func CheckPluginCommands(ctx context.Context, runtime deployment.Deployment, id string, plugins []Plugin) error {
	for _, plugin := range plugins {
		for _, server := range plugin.MCPServers {
			if server.Command == "" {
				continue
			}
			script := `command -v -- "$1" >/dev/null 2>&1`
			if strings.HasPrefix(server.Command, "/") {
				script = `test -f "$1" && test -x "$1"`
			}
			checkCtx, cancel := context.WithTimeout(ctx, commandCheckTimeout)
			result, err := runtime.Exec(checkCtx, id, core.Command{
				Path: "/bin/sh", Args: []string{"-c", script, "sh", server.Command}, Env: server.Env, Dir: server.Cwd,
			})
			cancel()
			if err != nil {
				return fmt.Errorf("check plugin %q MCP server %q command: %w", plugin.Name, server.Name, err)
			}
			if result.ExitCode != 0 {
				return fmt.Errorf("plugin %q MCP server %q: command %q is not available in the harness image", plugin.Name, server.Name, server.Command)
			}
		}
	}
	return nil
}

type pluginRecord struct {
	Name       string            `json:"name"`
	Version    string            `json:"version,omitempty"`
	Source     string            `json:"source"`
	TreeSHA256 string            `json:"tree_sha256"`
	StagedFile int               `json:"staged_files"`
	Bytes      int               `json:"staged_bytes"`
	Skills     []string          `json:"skills"`
	MCPServers []pluginServerRef `json:"mcp_servers"`
}

type pluginServerRef struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
}

// PluginRecord is the plugins.json evidence for one occurrence: which plugin
// packages were staged, from which pin, and what they contributed.
func PluginRecord(plugins []Plugin) ([]byte, error) {
	records := make([]pluginRecord, 0, len(plugins))
	for _, plugin := range plugins {
		record := pluginRecord{
			Name: plugin.Name, Version: plugin.Version, Source: plugin.Source, TreeSHA256: plugin.TreeSHA256,
			StagedFile: len(plugin.Files), Skills: append([]string{}, plugin.Skills...), MCPServers: []pluginServerRef{},
		}
		for _, file := range plugin.Files {
			record.Bytes += len(file.Content)
		}
		for _, server := range plugin.MCPServers {
			record.MCPServers = append(record.MCPServers, pluginServerRef{Name: server.Name, Command: server.Command, URL: server.URL})
		}
		records = append(records, record)
	}
	encoded, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode plugin record: %w", err)
	}
	return append(encoded, '\n'), nil
}
