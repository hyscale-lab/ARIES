package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `{
  "name":"test-run","versions_file":"../configs/versions.json",
  "benchmark":{"type":"terminalbench2","root":".cache/tb2","tasks":["fix-git"]},
  "harness":{"type":"openclaw"},"sandbox":{"type":"docker"},"bridge":{"type":"openclaw-ssh"},
  "runtime":{"backend":"deepseek","mode":"external"},
  "model":{"id":"fake","base_url":"http://127.0.0.1:8080","api_key_env":"DEEPSEEK_API_KEY"}
}`

const validVersions = `{"terminalbench2":{"repository_url":"https://example.invalid/terminal-bench-2.git","revision":"0123456789abcdef0123456789abcdef01234567"},"openclaw":{"image":"ghcr.io/openclaw/openclaw:2026.7.1"}}`

func TestNormalizedRuntimeSchema(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Backend != "deepseek" || cfg.Runtime.Mode != "external" || cfg.Model.ID != "fake" || cfg.CoreModel().Provider != "deepseek" || cfg.Execution.Concurrency != 1 {
		t.Fatalf("config = %#v", cfg)
	}
	managed := strings.Replace(validConfig, `"runtime":{"backend":"deepseek","mode":"external"}`, `"runtime":{"backend":"sglang","mode":"managed","config":{"file":"native.yaml","executable":"python3","startup_timeout":"15m","stop_timeout":"1m"}}`, 1)
	managed = strings.Replace(managed, `"id":"fake","base_url":"http://127.0.0.1:8080"`, `"id":"Qwen/Qwen3-8B","base_url":"http://host:30000/v1"`, 1)
	cfg, err = Decode(strings.NewReader(managed))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Config.StartupTimeout != 15*time.Minute || cfg.Runtime.Config.StopTimeout != time.Minute {
		t.Fatalf("runtime = %#v", cfg.Runtime)
	}
	external := strings.Replace(managed, `"mode":"managed","config":{"file":"native.yaml","executable":"python3","startup_timeout":"15m","stop_timeout":"1m"}`, `"mode":"external","config":{"file":"native.yaml"}`, 1)
	if _, err := Decode(strings.NewReader(external)); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsLegacyRuntimeFields(t *testing.T) {
	cases := map[string]string{
		"sglang_file":    strings.Replace(validConfig, `"versions_file":"../configs/versions.json",`, `"versions_file":"../configs/versions.json","sglang_file":"native.yaml",`, 1),
		"model_runtime":  strings.Replace(validConfig, `"runtime":`, `"model_runtime":{"mode":"external"},"runtime":`, 1),
		"model.provider": strings.Replace(validConfig, `"model":{"id":`, `"model":{"provider":"deepseek","id":`, 1),
		"model.model":    strings.Replace(validConfig, `"model":{"id":"fake",`, `"model":{"id":"fake","model":"fake",`, 1),
		"secret":         strings.Replace(validConfig, `"api_key_env":"DEEPSEEK_API_KEY"`, `"api_key_env":"DEEPSEEK_API_KEY","api_key":"secret"`, 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestRuntimeCombinationValidation(t *testing.T) {
	managed := strings.Replace(validConfig, `"runtime":{"backend":"deepseek","mode":"external"}`, `"runtime":{"backend":"sglang","mode":"managed","config":{"file":"native.yaml","executable":"python3","startup_timeout":"15m","stop_timeout":"1m"}}`, 1)
	managed = strings.Replace(managed, `"id":"fake","base_url":"http://127.0.0.1:8080"`, `"id":"Qwen/Qwen3-8B","base_url":"http://host:30000/v1"`, 1)
	invalid := []string{
		strings.Replace(validConfig, `"mode":"external"`, `"mode":"managed"`, 1),
		strings.Replace(validConfig, `"backend":"deepseek"`, `"backend":"other"`, 1),
		strings.Replace(validConfig, `"mode":"external"`, `"mode":"other"`, 1),
		strings.Replace(managed, `"executable":"python3"`, `"executable":""`, 1),
		strings.Replace(managed, `"file":"native.yaml"`, `"file":""`, 1),
		strings.Replace(managed, `"startup_timeout":"15m"`, `"startup_timeout":"0s"`, 1),
		strings.Replace(managed, `"stop_timeout":"1m"`, `"stop_timeout":"bad"`, 1),
	}
	for i, input := range invalid {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatalf("accepted invalid case %d", i)
		}
	}
}

func TestDecodeExecutionAndURLValidation(t *testing.T) {
	cfg, err := Decode(strings.NewReader(strings.Replace(validConfig, `"benchmark":`, `"execution":{"concurrency":5,"loop_duration":"250ms"},"benchmark":`, 1)))
	if err != nil || cfg.Execution.Concurrency != 5 || cfg.Execution.Loop != 250*time.Millisecond {
		t.Fatalf("execution=%#v err=%v", cfg.Execution, err)
	}
	sglang := strings.Replace(validConfig, `"runtime":{"backend":"deepseek","mode":"external"}`, `"runtime":{"backend":"sglang","mode":"external","config":{"file":"native.yaml"}}`, 1)
	sglang = strings.Replace(sglang, `http://127.0.0.1:8080`, `https://host:30000/v1/`, 1)
	cfg, err = Decode(strings.NewReader(sglang))
	if err != nil || cfg.Model.BaseURL != "https://host:30000/v1" {
		t.Fatalf("url=%q err=%v", cfg.Model.BaseURL, err)
	}
	for _, bad := range []string{"http://host/v1/v1", "http://host/v1?", "http://host/v%31"} {
		if _, err := Decode(strings.NewReader(strings.Replace(sglang, `https://host:30000/v1/`, bad, 1))); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestLoadResolvesRuntimeConfigAndVersions(t *testing.T) {
	root := t.TempDir()
	profiles := filepath.Join(root, "profiles")
	configs := filepath.Join(root, "configs")
	if err := os.MkdirAll(profiles, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configs, 0755); err != nil {
		t.Fatal(err)
	}
	profile := strings.Replace(validConfig, `"runtime":{"backend":"deepseek","mode":"external"}`, `"runtime":{"backend":"sglang","mode":"external","config":{"file":"../configs/native.yaml"}}`, 1)
	profile = strings.Replace(profile, `http://127.0.0.1:8080`, `http://host:30000/v1`, 1)
	if err := os.WriteFile(filepath.Join(profiles, "profile.json"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configs, "versions.json"), []byte(validVersions), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(profiles, "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.Abs(filepath.Join(configs, "native.yaml"))
	if cfg.Runtime.Config.ResolvedFile != want || cfg.Versions.OpenClaw.Image == "" {
		t.Fatalf("cfg=%#v", cfg)
	}
}

func TestCheckedInProfilesLoad(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "profiles", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 4 {
		t.Fatalf("profiles=%v", paths)
	}
	for _, path := range paths {
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if cfg.Runtime.Backend == "" || cfg.Runtime.Mode == "" || cfg.Model.ID == "" {
			t.Fatalf("%s: %#v", path, cfg)
		}
	}
}

func TestLoadRuntimeOverridesStrictSparseAndChecked(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	overrides, err := LoadRuntimeOverrides(write("valid.json", `{"harness_resources":{"cpu":1.25,"memory_mb":1024},"agent_sandbox_resources":{"cpu":2.5,"memory_mb":4096},"agent_timeout_seconds":12.5}`))
	if err != nil {
		t.Fatal(err)
	}
	if overrides.AgentTimeout == nil || *overrides.AgentTimeout != 12500*time.Millisecond {
		t.Fatalf("%#v", overrides)
	}
	for name, content := range map[string]string{"unknown": `{"future":1}`, "nested": `{"harness_resources":{"future":1}}`, "trailing": `{} {}`, "zero": `{"agent_sandbox_resources":{"cpu":0}}`, "overflow": `{"agent_timeout_seconds":1e999}`} {
		if _, err := LoadRuntimeOverrides(write(name+".json", content)); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	threshold := math.Exp2(63) / 1e9
	if _, err := LoadRuntimeOverrides(write("threshold.json", fmt.Sprintf(`{"harness_resources":{"cpu":%g}}`, threshold))); err == nil {
		t.Fatal("accepted overflow threshold")
	}
}

func TestDecodeVersionsValidation(t *testing.T) {
	if _, err := DecodeVersions(strings.NewReader(validVersions)); err != nil {
		t.Fatal(err)
	}
	cases := []string{strings.Replace(validVersions, `"image":`, `"future":true,"image":`, 1), strings.Replace(validVersions, "2026.7.1", "latest", 1), validVersions + ` {}`}
	for _, input := range cases {
		if _, err := DecodeVersions(strings.NewReader(input)); err == nil {
			t.Fatal("expected rejection")
		}
	}
}

func TestDecodeRejectsInvalidGenericFields(t *testing.T) {
	cases := []string{
		strings.Replace(validConfig, `"type":"docker"`, `"type":"docker","future":true`, 1),
		strings.Replace(validConfig, `"tasks":["fix-git"]`, `"tasks":[]`, 1),
		strings.Replace(validConfig, `"name":"test-run"`, `"name":"../escape"`, 1),
		strings.Replace(validConfig, "DEEPSEEK_API_KEY", "not-valid", 1),
		validConfig + ` {}`,
	}
	for _, input := range cases {
		if _, err := Decode(strings.NewReader(input)); err == nil {
			t.Fatal("expected rejection")
		}
	}
}
