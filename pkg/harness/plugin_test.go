package harness

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

func testPlugin() Plugin {
	return Plugin{
		Name: "demo", Version: "1.0.0", Source: "path /plugins/demo", TreeSHA256: strings.Repeat("a", 64),
		Files: []PluginFile{
			{Path: "plugin.json", Content: []byte("{}")},
			{Path: "skills/summarize/SKILL.md", Content: []byte("skill")},
			{Path: "bin/server", Content: []byte("#!/bin/sh\n"), Executable: true},
		},
		Skills: []string{"summarize"},
		MCPServers: []core.MCPServerConfig{
			{Name: "local", Command: "/run/aries/plugins/demo/bin/server"},
			{Name: "npx-server", Command: "npx"},
			{Name: "remote", URL: "https://mcp.example.com/mcp"},
		},
	}
}

func TestPluginArchiveStagesReadOnlyFilesAndAPrivateDataDirectory(t *testing.T) {
	directories, files := PluginArchive([]Plugin{testPlugin()}, 1000, 1000)
	archive, err := StageArchive(directories, files)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]int64{}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Uid != 1000 || header.Gid != 1000 {
			t.Fatalf("%s owned by %d:%d", header.Name, header.Uid, header.Gid)
		}
		modes[header.Name] = header.Mode
	}
	want := map[string]int64{
		"run/aries/plugin-data":                            0o755,
		"run/aries/plugin-data/demo":                       0o700,
		"run/aries/plugins":                                0o755,
		"run/aries/plugins/demo":                           0o755,
		"run/aries/plugins/demo/bin":                       0o755,
		"run/aries/plugins/demo/skills":                    0o755,
		"run/aries/plugins/demo/skills/summarize":          0o755,
		"run/aries/plugins/demo/plugin.json":               0o444,
		"run/aries/plugins/demo/skills/summarize/SKILL.md": 0o444,
		"run/aries/plugins/demo/bin/server":                0o555,
	}
	if !reflect.DeepEqual(modes, want) {
		t.Fatalf("staged modes = %v, want %v", modes, want)
	}
	if dirs, files := PluginArchive(nil, 1000, 1000); dirs != nil || files != nil {
		t.Fatal("no plugins must stage nothing")
	}
}

func TestPluginSkillDirsListsOnlyPluginsWithSkills(t *testing.T) {
	withSkills, without := testPlugin(), testPlugin()
	without.Name, without.Skills = "tools-only", nil
	if got := PluginSkillDirs([]Plugin{without, withSkills}); !reflect.DeepEqual(got, []string{"/run/aries/plugins/demo/skills"}) {
		t.Fatalf("skill dirs = %v", got)
	}
}

func TestPluginRecordNamesPinContentAndComponents(t *testing.T) {
	record, err := PluginRecord([]Plugin{testPlugin()})
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(record, &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded[0]
	if got["name"] != "demo" || got["source"] != "path /plugins/demo" || got["tree_sha256"] != strings.Repeat("a", 64) ||
		got["staged_files"] != float64(3) || got["staged_bytes"] != float64(17) || len(got["mcp_servers"].([]any)) != 3 {
		t.Fatalf("record = %s", record)
	}
}

type execDeployment struct {
	deployment.Deployment
	missing  map[string]bool
	commands []string
	executed []core.Command
}

func (d *execDeployment) Exec(_ context.Context, id string, command core.Command) (core.CommandResult, error) {
	if id != "runtime" || command.Path != "/bin/sh" || len(command.Args) != 4 {
		return core.CommandResult{}, io.ErrUnexpectedEOF
	}
	target := command.Args[3]
	d.commands = append(d.commands, target)
	d.executed = append(d.executed, command)
	if d.missing[target] {
		return core.CommandResult{ExitCode: 1}, nil
	}
	return core.CommandResult{}, nil
}

func TestCheckPluginCommandsChecksEveryCommandServer(t *testing.T) {
	runtime := &execDeployment{}
	if err := CheckPluginCommands(context.Background(), runtime, "runtime", []Plugin{testPlugin()}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runtime.commands, []string{"/run/aries/plugins/demo/bin/server", "npx"}) {
		t.Fatalf("checked %v; url servers are not commands", runtime.commands)
	}
	runtime = &execDeployment{missing: map[string]bool{"npx": true}}
	err := CheckPluginCommands(context.Background(), runtime, "runtime", []Plugin{testPlugin()})
	if err == nil || !strings.Contains(err.Error(), `command "npx" is not available in the harness image`) {
		t.Fatalf("missing command error = %v", err)
	}
}

// A package may put its own bin directory on the server's PATH; the lookup
// runs with the environment and working directory the server is launched with.
func TestCheckPluginCommandsUsesTheServerEnvironment(t *testing.T) {
	plugin := testPlugin()
	plugin.MCPServers = []core.MCPServerConfig{{
		Name: "bundled", Command: "server",
		Env: map[string]string{"PATH": "/run/aries/plugins/demo/bin:/usr/bin:/bin"}, Cwd: "/run/aries/plugins/demo",
	}}
	runtime := &execDeployment{}
	if err := CheckPluginCommands(context.Background(), runtime, "runtime", []Plugin{plugin}); err != nil {
		t.Fatal(err)
	}
	got := runtime.executed[0]
	if got.Env["PATH"] != "/run/aries/plugins/demo/bin:/usr/bin:/bin" || got.Dir != "/run/aries/plugins/demo" {
		t.Fatalf("lookup ran with env %v in %q", got.Env, got.Dir)
	}
}
