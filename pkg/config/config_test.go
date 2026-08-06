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

const validVersions = `{"terminalbench2":{"repository_url":"https://example.invalid/terminal-bench-2.git","revision":"0123456789abcdef0123456789abcdef01234567"},"openclaw":{"image":"ghcr.io/openclaw/openclaw:2026.7.1"},"hermes":{"image":"docker.io/nousresearch/hermes-agent:v2026.5.29.2"}}`

func TestNormalizedRuntimeSchema(t *testing.T) {
	cfg, err := Decode(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Runtime.Backend != "deepseek" || cfg.Runtime.Mode != "external" || cfg.Model.ID != "fake" || cfg.CoreModel().Provider != "deepseek" || cfg.Execution.Concurrency != 1 {
		t.Fatalf("config = %#v", cfg)
	}
	if cfg.Harness.Mode != "agent" {
		t.Fatalf("harness mode = %q, want agent", cfg.Harness.Mode)
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

func TestRealtimeHarnessConfigValidationAndResolution(t *testing.T) {
	realtime := strings.Replace(validConfig, `"harness":{"type":"openclaw"}`, `"harness":{"type":"openclaw","mode":"realtime","realtime":{"tts":{"provider":"openai","model":"gpt-4o-mini-tts","voice":"alloy","timeout":"2s","speed":1.1},"chunk_duration":"25ms","listen_duration":"3s","quiet_duration":"250ms","agent_wait_duration":"2s","tool_call_timeout":"1s","trailing_silence_ms":300,"voice":"alloy","reasoning_effort":"low","include_events":true}}`, 1)
	cfg, err := Decode(strings.NewReader(realtime))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Mode != "realtime" || cfg.Harness.Realtime.ChunkDuration != 25*time.Millisecond || cfg.Harness.Realtime.ListenDuration != 3*time.Second || cfg.Harness.Realtime.TrailingSilenceMillis != 300 || !cfg.Harness.Realtime.IncludeEvents || cfg.Harness.Realtime.TTS.APIKeyEnv != "OPENAI_API_KEY" || cfg.Harness.Realtime.TTS.Timeout != 2*time.Second {
		t.Fatalf("harness realtime = %#v", cfg.Harness)
	}

	for name, input := range map[string]string{
		"bad mode":       strings.Replace(validConfig, `"harness":{"type":"openclaw"}`, `"harness":{"type":"openclaw","mode":"other"}`, 1),
		"agent realtime": strings.Replace(validConfig, `"harness":{"type":"openclaw"}`, `"harness":{"type":"openclaw","mode":"agent","realtime":{"audio_path":"audio.wav"}}`, 1),
		"audio path":     strings.Replace(realtime, `"tts":{"provider":"openai","model":"gpt-4o-mini-tts","voice":"alloy","timeout":"2s","speed":1.1}`, `"audio_path":"audio.wav","tts":{"provider":"openai","model":"gpt-4o-mini-tts","voice":"alloy","timeout":"2s","speed":1.1}`, 1),
		"bad duration":   strings.Replace(realtime, `"chunk_duration":"25ms"`, `"chunk_duration":"0s"`, 1),
		"bad silence":    strings.Replace(realtime, `"trailing_silence_ms":300`, `"trailing_silence_ms":-1`, 1),
		"bad tts":        strings.Replace(realtime, `"provider":"openai"`, `"provider":"elevenlabs"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(input)); err == nil {
				t.Fatal("expected rejection")
			}
		})
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
	if len(paths) != 6 {
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
		if strings.Contains(path, "realtime") {
			if cfg.Harness.Mode != "realtime" || cfg.Harness.Realtime.TTS.APIKeyEnv != "OPENAI_API_KEY" || cfg.Harness.Realtime.ChunkDuration != 50*time.Millisecond {
				t.Fatalf("%s realtime harness: %#v", path, cfg.Harness)
			}
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

// A catalog written before a harness existed must keep loading, so an absent
// image is only an error for the harness that actually needs it. An image that
// is present is still pin-validated.
func TestVersionsRequireOnlyTheSelectedHarnessImage(t *testing.T) {
	withoutHermes := `{"terminalbench2":{"repository_url":"https://example.invalid/terminal-bench-2.git","revision":"0123456789abcdef0123456789abcdef01234567"},"openclaw":{"image":"ghcr.io/openclaw/openclaw:2026.7.1"}}`
	versions, err := DecodeVersions(strings.NewReader(withoutHermes))
	if err != nil {
		t.Fatalf("catalog without hermes.image was rejected: %v", err)
	}
	if image, err := versions.HarnessImage("openclaw"); err != nil || image != "ghcr.io/openclaw/openclaw:2026.7.1" {
		t.Fatalf("openclaw image = %q, %v", image, err)
	}
	if _, err := versions.HarnessImage("hermes"); err == nil {
		t.Fatal("missing hermes.image was accepted for the hermes harness")
	}
	if _, err := versions.HarnessImage("nope"); err == nil {
		t.Fatal("unknown harness type was accepted")
	}

	full, err := DecodeVersions(strings.NewReader(validVersions))
	if err != nil {
		t.Fatal(err)
	}
	if image, err := full.HarnessImage("hermes"); err != nil || image != "docker.io/nousresearch/hermes-agent:v2026.5.29.2" {
		t.Fatalf("hermes image = %q, %v", image, err)
	}
	unpinned := strings.Replace(validVersions, "hermes-agent:v2026.5.29.2", "hermes-agent:latest", 1)
	if _, err := DecodeVersions(strings.NewReader(unpinned)); err == nil {
		t.Fatal("unpinned hermes.image was accepted")
	}
}

func TestDecodeRejectsInvalidGenericFields(t *testing.T) {
	cases := []string{
		strings.Replace(validConfig, `"type":"docker"`, `"type":"docker","future":true`, 1),
		strings.Replace(validConfig, `"benchmark":`, `"monitor":{"future":true},"benchmark":`, 1),
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
