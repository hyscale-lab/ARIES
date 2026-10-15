package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const codexVersionSection = `"codex":{"image":"docker.io/library/debian:12.12-slim","version":"0.157.1"}`

func TestDecodeCodexSupportsResponsesBackendsAndContextLength(t *testing.T) {
	for _, backend := range []string{"openai", "sglang"} {
		input := strings.Replace(codexProfileJSON(), `"backend":"openai"`, `"backend":"`+backend+`"`, 1)
		cfg, err := Decode(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Harness.Mode != "agent" || cfg.Harness.Codex == nil || cfg.Harness.Codex.Executable != "../.cache/codex/0.157.1/codex" {
			t.Fatalf("Codex harness = %#v", cfg.Harness)
		}
		if cfg.Harness.Subagents.Enabled != nil || cfg.Harness.WebSearch.Enabled {
			t.Fatal("Codex inherited unsupported harness features")
		}
		if model := cfg.CoreModel(); model.Provider != backend || model.ContextLength != 32768 || model.MaxTokens != 0 || model.Temperature != nil {
			t.Fatalf("Codex model = %#v", model)
		}
	}
}

func TestDecodeCodexRejectsUnsupportedSettings(t *testing.T) {
	base := codexProfileJSON()
	for name, input := range map[string]string{
		"missing codex block": strings.Replace(base, `,"codex":{"executable":"../.cache/codex/0.157.1/codex"}`, "", 1),
		"empty executable":    strings.Replace(base, `"../.cache/codex/0.157.1/codex"`, `""`, 1),
		"NUL executable":      strings.Replace(base, `"../.cache/codex/0.157.1/codex"`, `"bad\u0000path"`, 1),
		"block under Hermes":  strings.Replace(base, `"type":"codex"`, `"type":"hermes"`, 1),
		"deepseek backend":    strings.Replace(base, `"backend":"openai"`, `"backend":"deepseek"`, 1),
		"negative context":    strings.Replace(base, `"context_length":32768`, `"context_length":-1`, 1),
		"max tokens":          strings.Replace(base, `"context_length":32768`, `"context_length":32768,"max_tokens":512`, 1),
		"zero temperature":    strings.Replace(base, `"context_length":32768`, `"context_length":32768,"temperature":0`, 1),
		"web search":          strings.Replace(base, `"type":"codex"`, `"type":"codex","web_search":{"enabled":true}`, 1),
		"subagents":           strings.Replace(base, `"type":"codex"`, `"type":"codex","subagents":{"enabled":false}`, 1),
		"compaction":          strings.Replace(base, `"type":"codex"`, `"type":"codex","compaction":{"enabled":false}`, 1),
		"realtime mode":       strings.Replace(base, `"type":"codex"`, `"type":"codex","mode":"realtime"`, 1),
		"voice mode":          strings.Replace(base, `"type":"codex"`, `"type":"codex","mode":"voice-transcribe"`, 1),
		"unknown codex field": strings.Replace(base, `"codex":{"executable":`, `"codex":{"shell":"ssh","executable":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if input == base {
				t.Fatal("test mutation did not change the profile")
			}
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("unsupported Codex profile was accepted")
			}
		})
	}
}

func TestCodexVersionCatalogRemainsOptionalAndPinned(t *testing.T) {
	legacy, err := DecodeVersions(strings.NewReader(validVersions))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.HarnessImage("openclaw"); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.HarnessImage("codex"); err == nil {
		t.Fatal("Codex accepted a catalog without its version section")
	}
	catalog := strings.TrimSuffix(validVersions, "}") + "," + codexVersionSection + "}"
	versions, err := DecodeVersions(strings.NewReader(catalog))
	if err != nil {
		t.Fatal(err)
	}
	if image, err := versions.HarnessImage("codex"); err != nil || image != "docker.io/library/debian:12.12-slim" || versions.Codex.Version != "0.157.1" {
		t.Fatalf("Codex pins = %#v; image = %q, err = %v", versions.Codex, image, err)
	}
	for _, version := range []string{"", "latest", "v0.157.1", "0.157", "0.157.*", "00.157.1", "0.157.1-alpha", "0.157.1+local", " 0.157.1"} {
		t.Run("version "+version, func(t *testing.T) {
			input := strings.Replace(catalog, `"version":"0.157.1"`, `"version":"`+version+`"`, 1)
			if _, err := DecodeVersions(strings.NewReader(input)); err == nil {
				t.Fatalf("unpinned version %q was accepted", version)
			}
		})
	}
	for _, input := range []string{
		strings.Replace(catalog, "debian:12.12-slim", "debian:latest", 1),
		strings.Replace(catalog, `"version":"0.157.1"`, `"version":"0.157.1","revision":"ignored"`, 1),
	} {
		if _, err := DecodeVersions(strings.NewReader(input)); err == nil {
			t.Fatal("invalid Codex version catalog was accepted")
		}
	}
}

func TestLoadCodexResolvesExecutableAndRequiresSelectedVersion(t *testing.T) {
	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	if err := os.Mkdir(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	versionsPath := filepath.Join(root, "versions.json")
	catalog := strings.TrimSuffix(validVersions, "}") + "," + codexVersionSection + "}"
	if err := os.WriteFile(versionsPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{"../.cache/codex/0.157.1/codex", filepath.Join(root, "binary with spaces", "codex")} {
		encoded, err := json.Marshal(executable)
		if err != nil {
			t.Fatal(err)
		}
		input := strings.Replace(codexProfileJSON(), "../configs/versions.json", "../versions.json", 1)
		input = strings.Replace(input, `"../.cache/codex/0.157.1/codex"`, string(encoded), 1)
		profilePath := filepath.Join(profiles, "codex.json")
		if err := os.WriteFile(profilePath, []byte(input), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(profilePath)
		if err != nil {
			t.Fatal(err)
		}
		want := executable
		if !filepath.IsAbs(want) {
			want = filepath.Join(profiles, want)
		}
		if cfg.Harness.Codex.ResolvedExecutable != want || cfg.Harness.Codex.Executable != executable {
			t.Fatalf("Codex executable = %#v, want resolved %q", cfg.Harness.Codex, want)
		}
	}
	if err := os.WriteFile(versionsPath, []byte(validVersions), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(profiles, "codex.json")); err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("Codex Load accepted missing version pins: %v", err)
	}
}

func codexProfileJSON() string {
	input := strings.Replace(validConfig, `"harness":{"type":"openclaw"}`, `"harness":{"type":"codex","codex":{"executable":"../.cache/codex/0.157.1/codex"}}`, 1)
	input = strings.Replace(input, `"type":"openclaw-ssh"`, `"type":"codex-ssh"`, 1)
	input = strings.Replace(input, `"backend":"deepseek"`, `"backend":"openai"`, 1)
	input = strings.Replace(input, `"http://127.0.0.1:8080"`, `"http://model.local:8000/v1"`, 1)
	return strings.Replace(input, `"id":"fake"`, `"id":"fake","context_length":32768`, 1)
}
