package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pluginRevision = "4444444444444444444444444444444444444444"

func versionsWithPlugins(plugins string) string {
	return strings.TrimSuffix(validVersions, "}") + `,"plugins":` + plugins + `}`
}

func profileWithPlugins(plugins string) string {
	return strings.Replace(validConfig, `"harness":{"type":"openclaw"}`, `"harness":{"type":"openclaw","plugins":`+plugins+`}`, 1)
}

func TestPluginVersionsValidation(t *testing.T) {
	valid := map[string]string{
		"repository":              `{"demo":{"repository_url":"https://example.invalid/demo.git","revision":"` + pluginRevision + `"}}`,
		"repository subdirectory": `{"demo":{"repository_url":"https://example.invalid/demo.git","revision":"` + pluginRevision + `","subdirectory":"plugins/demo"}}`,
		"local path":              `{"demo":{"path":"../plugins/demo"}}`,
		"name with a period":      `{"acme.tools":{"path":"plugins/acme"}}`,
	}
	for name, plugins := range valid {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeVersions(strings.NewReader(versionsWithPlugins(plugins))); err != nil {
				t.Fatal(err)
			}
		})
	}
	invalid := map[string]struct{ plugins, wantErr string }{
		"no source":              {`{"demo":{}}`, "set repository_url and revision, or path"},
		"both sources":           {`{"demo":{"path":"p","repository_url":"https://example.invalid/demo.git","revision":"` + pluginRevision + `"}}`, "not both"},
		"short revision":         {`{"demo":{"repository_url":"https://example.invalid/demo.git","revision":"abc"}}`, "40-character Git revision"},
		"escaping subdirectory":  {`{"demo":{"repository_url":"https://example.invalid/demo.git","revision":"` + pluginRevision + `","subdirectory":"../x"}}`, "subdirectory must be a clean relative path"},
		"subdirectory on a path": {`{"demo":{"path":"p","subdirectory":"x"}}`, "applies only to a repository"},
		"unclean path":           {`{"demo":{"path":"plugins/../demo"}}`, "path must be a clean path"},
		"invalid name":           {`{"Demo":{"path":"p"}}`, "must be an Agent Plugins name"},
	}
	for name, testCase := range invalid {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeVersions(strings.NewReader(versionsWithPlugins(testCase.plugins)))
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, testCase.wantErr)
			}
		})
	}
}

func TestHarnessPluginsValidation(t *testing.T) {
	if _, err := Decode(strings.NewReader(profileWithPlugins(`[{"name":"demo","secret_env":{"MEMORY_KEY":"HOST_MEMORY_KEY"}}]`))); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct{ profile, wantErr string }{
		"other harness": {
			strings.Replace(profileWithPlugins(`[{"name":"demo"}]`), `"type":"openclaw"`, `"type":"other"`, 1),
			"harness.plugins requires OpenClaw or Hermes",
		},
		"selected twice":      {profileWithPlugins(`[{"name":"demo"},{"name":"demo"}]`), "selected twice"},
		"invalid name":        {profileWithPlugins(`[{"name":"Demo"}]`), "is not an Agent Plugins name"},
		"bad secret_env name": {profileWithPlugins(`[{"name":"demo","secret_env":{"MEMORY KEY":"HOST"}}]`), "must map environment variable names"},
		"reserved secret_env": {profileWithPlugins(`[{"name":"demo","secret_env":{"PLUGIN_DATA":"HOST"}}]`), "must not set PLUGIN_DATA"},
		"unknown entry field": {profileWithPlugins(`[{"name":"demo","config":{}}]`), "unknown field"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(testCase.profile))
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, testCase.wantErr)
			}
		})
	}
}

// Load resolves a local plugin path against the versions file and requires
// every selected plugin to be pinned.
func TestLoadResolvesPluginPathsAndRequiresPins(t *testing.T) {
	root := t.TempDir()
	profiles, configs := filepath.Join(root, "profiles"), filepath.Join(root, "configs")
	for _, dir := range []string{profiles, configs} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	versions := versionsWithPlugins(`{"demo":{"path":"../plugins/demo"}}`)
	if err := os.WriteFile(filepath.Join(configs, "versions.json"), []byte(versions), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(plugins string) string {
		path := filepath.Join(profiles, "profile.json")
		if err := os.WriteFile(path, []byte(profileWithPlugins(plugins)), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg, err := Load(write(`[{"name":"demo"}]`))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(filepath.Join(root, "plugins", "demo"))
	if got := cfg.Versions.Plugins["demo"].ResolvedPath; got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}
	if _, err := Load(write(`[{"name":"other"}]`)); err == nil || !strings.Contains(err.Error(), "not pinned in the version catalog") {
		t.Fatalf("unpinned plugin error = %v", err)
	}
}
