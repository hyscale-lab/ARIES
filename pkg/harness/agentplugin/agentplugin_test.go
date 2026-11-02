package agentplugin

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const manifest = `{"$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json", "name": "demo", "version": "1.2.0"}`

const skill = "---\nname: summarize\ndescription: Summarize a file.\n---\nRead the file and summarize it.\n"

const mcpConfig = `{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json",
  "mcpServers": {
    "local": {"type": "stdio", "command": "./bin/server", "args": ["--data", "${PLUGIN_DATA}/db"], "env": {"CONFIG": "${PLUGIN_ROOT}/config.json"}},
    "remote": {"type": "streamable-http", "url": "https://mcp.example.com/mcp", "headers": {"X-Tenant": "public"}}
  }
}`

// writePackage creates a package from path -> content; a path ending in
// "*" is written executable (the star is dropped).
func writePackage(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "*") {
			name, mode = strings.TrimSuffix(name, "*"), 0o755
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func validPackage() map[string]string {
	return map[string]string{
		"plugin.json":                manifest,
		"skills/summarize/SKILL.md":  skill,
		"skills/summarize/scripts/a": "echo a",
		"mcp.json":                   mcpConfig,
		"bin/server*":                "#!/bin/sh\n",
		"config.json":                "{}",
		".git/HEAD":                  "ref",
		"skills/README.md":           "not a skill",
		"skills/notes/readme.md":     "no SKILL.md, not a skill",
	}
}

func TestLoadTranslatesAPackage(t *testing.T) {
	plugin, err := Load(writePackage(t, validPackage()), "demo", "https://example.com/demo@rev")
	if err != nil {
		t.Fatal(err)
	}
	if plugin.Name != "demo" || plugin.Version != "1.2.0" || plugin.Source != "https://example.com/demo@rev" || len(plugin.TreeSHA256) != 64 {
		t.Fatalf("plugin identity = %+v", plugin)
	}
	if !reflect.DeepEqual(plugin.Skills, []string{"summarize"}) {
		t.Fatalf("skills = %v", plugin.Skills)
	}
	for _, file := range plugin.Files {
		if strings.HasPrefix(file.Path, ".git/") {
			t.Fatalf("version control file staged: %s", file.Path)
		}
		if file.Path == "bin/server" && !file.Executable {
			t.Fatal("executable bit lost")
		}
	}
	if len(plugin.MCPServers) != 2 {
		t.Fatalf("servers = %+v", plugin.MCPServers)
	}
	local, remote := plugin.MCPServers[0], plugin.MCPServers[1]
	if local.Command != "/run/aries/plugins/demo/bin/server" || local.Cwd != "/run/aries/plugins/demo" {
		t.Fatalf("local command/cwd = %q %q", local.Command, local.Cwd)
	}
	if !reflect.DeepEqual(local.Args, []string{"--data", "/run/aries/plugin-data/demo/db"}) {
		t.Fatalf("local args = %v", local.Args)
	}
	wantEnv := map[string]string{"CONFIG": "/run/aries/plugins/demo/config.json", "PLUGIN_ROOT": "/run/aries/plugins/demo", "PLUGIN_DATA": "/run/aries/plugin-data/demo"}
	if !reflect.DeepEqual(local.Env, wantEnv) {
		t.Fatalf("local env = %v", local.Env)
	}
	if remote.URL != "https://mcp.example.com/mcp" || remote.Transport != "streamable-http" || remote.Headers["X-Tenant"] != "public" {
		t.Fatalf("remote = %+v", remote)
	}
}

func TestLoadHashFollowsContent(t *testing.T) {
	files := validPackage()
	first, err := Load(writePackage(t, files), "demo", "")
	if err != nil {
		t.Fatal(err)
	}
	same, err := Load(writePackage(t, files), "demo", "")
	if err != nil {
		t.Fatal(err)
	}
	files["config.json"] = `{"changed": true}`
	changed, err := Load(writePackage(t, files), "demo", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.TreeSHA256 != same.TreeSHA256 || first.TreeSHA256 == changed.TreeSHA256 {
		t.Fatalf("hashes: %s %s %s", first.TreeSHA256, same.TreeSHA256, changed.TreeSHA256)
	}
}

func TestLoadRejectsInvalidPackages(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(map[string]string)
		wantErr string
	}{
		{"missing manifest", func(f map[string]string) { delete(f, "plugin.json") }, "plugin.json is missing"},
		{"wrong schema", func(f map[string]string) { f["plugin.json"] = `{"$schema": "https://example.com/x", "name": "demo"}` }, "$schema must be"},
		{"catalog name mismatch", func(f map[string]string) {
			f["plugin.json"] = strings.Replace(manifest, `"demo"`, `"other"`, 1)
		}, "does not match the catalog name"},
		{"invalid manifest name", func(f map[string]string) {
			f["plugin.json"] = strings.Replace(manifest, `"demo"`, `"Demo"`, 1)
		}, "name must be"},
		{"author with an unknown field", func(f map[string]string) {
			f["plugin.json"] = strings.Replace(manifest, `}`, `, "author": {"twitter": "x"}}`, 1)
		}, "author field"},
		{"skill name differs from its directory", func(f map[string]string) {
			f["skills/summarize/SKILL.md"] = strings.Replace(skill, "name: summarize", "name: other", 1)
		}, "must match its directory"},
		{"skill without description", func(f map[string]string) {
			f["skills/summarize/SKILL.md"] = "---\nname: summarize\n---\nbody\n"
		}, "description must be"},
		{"unknown mcp server type", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, `"type": "stdio"`, `"type": "ws"`, 1)
		}, "is not stdio"},
		{"field of another variant", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, `"command": "./bin/server"`, `"command": "./bin/server", "url": "https://x"`, 1)
		}, `field "url" does not belong`},
		{"command outside the package", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, "./bin/server", "./../server", 1)
		}, "leaves the package"},
		{"command path without ./", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, "./bin/server", "bin/server", 1)
		}, "bare name or a ./ path"},
		{"command not executable", func(f map[string]string) {
			delete(f, "bin/server*")
			f["bin/server"] = "#!/bin/sh\n"
		}, "not an executable file"},
		{"cwd escapes the data directory", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, `"args"`, `"cwd": "${PLUGIN_DATA}/../x", "args"`, 1)
		}, "leaves its base directory"},
		{"other placeholder", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, "${PLUGIN_ROOT}/config.json", "${DEEPSEEK_API_KEY}", 1)
		}, "contains a ${...} reference"},
		{"placeholder as the command", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, `"./bin/server"`, `"${HOME}"`, 1)
		}, "command contains a ${...} reference"},
		{"placeholder left in cwd", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, `"args"`, `"cwd": "./${DATA_DIR}", "args"`, 1)
		}, "cwd contains a ${...} reference"},
		{"plain http to a remote host", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, "https://mcp.example.com/mcp", "http://mcp.example.com/mcp", 1)
		}, "HTTPS unless its host is loopback"},
		{"credential header", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, `"X-Tenant"`, `"Authorization"`, 1)
		}, "carries a credential"},
		{"env sets a reserved name", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, `"CONFIG"`, `"PLUGIN_ROOT"`, 1)
		}, "must not set PLUGIN_ROOT"},
		{"extra top-level mcp field", func(f map[string]string) {
			f["mcp.json"] = strings.Replace(mcpConfig, `"mcpServers"`, `"extra": 1, "mcpServers"`, 1)
		}, `top-level field "extra"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := validPackage()
			tt.edit(files)
			_, err := Load(writePackage(t, files), "demo", "")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRefusesSymbolicLinks(t *testing.T) {
	root := writePackage(t, validPackage())
	if err := os.Symlink("/etc/hostname", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root, "demo", ""); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("Load() error = %v, want a symbolic link refusal", err)
	}
}

// The Agent Skills limit is 1024 characters, not bytes.
func TestSkillDescriptionIsCountedInCharacters(t *testing.T) {
	files := validPackage()
	files["skills/summarize/SKILL.md"] = "---\nname: summarize\ndescription: " + strings.Repeat("概", 400) + "\n---\nbody\n"
	if _, err := Load(writePackage(t, files), "demo", ""); err != nil {
		t.Fatalf("400 CJK characters (1200 bytes) must be accepted: %v", err)
	}
	files["skills/summarize/SKILL.md"] = "---\nname: summarize\ndescription: " + strings.Repeat("a", 1025) + "\n---\nbody\n"
	if _, err := Load(writePackage(t, files), "demo", ""); err == nil || !strings.Contains(err.Error(), "description must be") {
		t.Fatalf("1025 characters must be rejected, got %v", err)
	}
}

func TestLoadAcceptsTheProbePackage(t *testing.T) {
	plugin, err := Load(filepath.Join("testdata", "aries-probe"), "aries-probe", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plugin.Skills, []string{"aries-probe-skill"}) || len(plugin.MCPServers) != 1 {
		t.Fatalf("probe = %+v", plugin)
	}
	server := plugin.MCPServers[0]
	if server.Command != "node" || !reflect.DeepEqual(server.Args, []string{"/run/aries/plugins/aries-probe/server.mjs"}) || server.Env["ARIES_PROBE_MARKER"] != "portable-probe" {
		t.Fatalf("probe server = %+v", server)
	}
}

func TestLoadIgnoresUnknownManifestFields(t *testing.T) {
	files := validPackage()
	files["plugin.json"] = strings.Replace(manifest, `}`, `, "commands": ["x"]}`, 1)
	if _, err := Load(writePackage(t, files), "demo", ""); err != nil {
		t.Fatalf("an unknown manifest field must be ignored (§5.2): %v", err)
	}
}
