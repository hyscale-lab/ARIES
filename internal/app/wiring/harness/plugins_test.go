package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
)

// writePluginPackage writes a minimal package named demo whose skill body is
// body.
func writePluginPackage(t *testing.T, root, body string) {
	t.Helper()
	files := map[string]string{
		"plugin.json":               `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "demo", "version": "1.0.0"}`,
		"skills/summarize/SKILL.md": "---\nname: summarize\ndescription: Summarize a file.\n---\n" + body + "\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A pinned subdirectory that is a symbolic link out of its checkout is
// refused before any of its files are read: they would be recorded as the
// pinned commit's.
func TestLoadPluginsRefusesASubdirectoryOutsideItsCheckout(t *testing.T) {
	t.Chdir(t.TempDir())
	outside := t.TempDir()
	writePluginPackage(t, outside, "Read the file and summarize it.")
	revision := strings.Repeat("a", 40)
	checkout := filepath.Join(pluginCacheRoot, "demo@"+revision)
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(checkout, "plugin")); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Harness.Plugins = []config.HarnessPluginConfig{{Name: "demo"}}
	cfg.Versions.Plugins = map[string]config.PluginVersions{"demo": {RepositoryURL: "https://example.com/demo.git", Revision: revision, Subdirectory: "plugin"}}
	if _, err := LoadPlugins(cfg); err == nil || !strings.Contains(err.Error(), "leaves the checkout") {
		t.Fatalf("LoadPlugins = %v, want a refusal of a subdirectory outside the checkout", err)
	}
	// A link that stays inside the checkout is followed, even by an absolute path.
	inside, err := filepath.Abs(filepath.Join(checkout, "package"))
	if err != nil {
		t.Fatal(err)
	}
	writePluginPackage(t, inside, "Read the file and summarize it.")
	if err := os.Remove(filepath.Join(checkout, "plugin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(inside, filepath.Join(checkout, "plugin")); err != nil {
		t.Fatal(err)
	}
	if plugins, err := LoadPlugins(cfg); err != nil || len(plugins) != 1 {
		t.Fatalf("LoadPlugins with a subdirectory inside the checkout = %v, %v", plugins, err)
	}
}

// A later task stages the package that the first read accepted, even if the
// pinned subdirectory's link is changed in the cache during the run.
func TestLaterTasksStageTheAcceptedSubdirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	revision := strings.Repeat("b", 40)
	checkout := filepath.Join(pluginCacheRoot, "demo@"+revision)
	writePluginPackage(t, filepath.Join(checkout, "first"), "Version one.")
	writePluginPackage(t, filepath.Join(checkout, "second"), "Version two.")
	link := filepath.Join(checkout, "plugin")
	if err := os.Symlink("first", link); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}
	cfg.Harness.Plugins = []config.HarnessPluginConfig{{Name: "demo"}}
	cfg.Versions.Plugins = map[string]config.PluginVersions{"demo": {RepositoryURL: "https://example.com/demo.git", Revision: revision, Subdirectory: "plugin"}}
	first, err := LoadPlugins(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("second", link); err != nil {
		t.Fatal(err)
	}
	later, err := LoadPlugins(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].TreeSHA256 != later[0].TreeSHA256 {
		t.Fatalf("a later task staged another package: %s, then %s", first[0].TreeSHA256, later[0].TreeSHA256)
	}
}

// Every task of a run stages the same files, even if the package changes on
// disk while the run goes on.
func TestEveryTaskOfARunStagesTheSameFiles(t *testing.T) {
	root := t.TempDir()
	writePluginPackage(t, root, "Version one.")
	cfg := config.Config{}
	cfg.Harness.Plugins = []config.HarnessPluginConfig{{Name: "demo"}}
	cfg.Versions.Plugins = map[string]config.PluginVersions{"demo": {Path: root, ResolvedPath: root}}
	var first, second harnesscommon.Options
	if err := pluginOptions(cfg, &first); err != nil {
		t.Fatal(err)
	}
	writePluginPackage(t, root, "Version two.")
	if err := pluginOptions(cfg, &second); err != nil {
		t.Fatal(err)
	}
	if first.Plugins[0].TreeSHA256 != second.Plugins[0].TreeSHA256 {
		t.Fatalf("two tasks staged different trees: %s and %s", first.Plugins[0].TreeSHA256, second.Plugins[0].TreeSHA256)
	}
}

func TestWithPluginServersAppendsAndRefusesSharedNames(t *testing.T) {
	profile := []core.MCPServerConfig{{Name: "docs", URL: "https://docs.example/mcp"}}
	plugins := []harnesscommon.Plugin{
		{Name: "memory", MCPServers: []core.MCPServerConfig{{Name: "memory", Command: "npx"}}},
	}
	servers, err := withPluginServers(profile, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Name != "docs" || servers[1].Name != "memory" {
		t.Fatalf("servers = %+v", servers)
	}
	if len(profile) != 1 {
		t.Fatal("the profile's server list must not be modified")
	}
	clash := append(plugins, harnesscommon.Plugin{Name: "other", MCPServers: []core.MCPServerConfig{{Name: "docs", Command: "node"}}})
	if _, err := withPluginServers(profile, clash); err == nil || !strings.Contains(err.Error(), `plugin "other" MCP server "docs" has the name of a server from harness.mcp_servers`) {
		t.Fatalf("clash error = %v", err)
	}
}
