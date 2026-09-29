package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const roadmapBenchVersionSection = `"roadmapbench":{"repository_url":"https://huggingface.co/datasets/UnipatAI/RoadmapBench","revision":"59184e779909300a5a0150b06b945d39da81a099"}`

func TestRoadmapBenchRejectsUnrelatedBenchmarkBlocks(t *testing.T) {
	base := strings.Replace(validConfig, `"type":"terminalbench2"`, `"type":"roadmapbench"`, 1)
	if _, err := Decode(strings.NewReader(base)); err != nil {
		t.Fatalf("valid roadmapbench profile: %v", err)
	}
	for name, block := range map[string]string{
		"environment": `,"environment":{"image":"x"}`,
		"judge":       `,"judge":{"enabled":false}`,
		"fact":        `,"fact":{"jina_api_key_env":"JINA_API_KEY"}`,
	} {
		t.Run(name, func(t *testing.T) {
			input := strings.Replace(base, `"tasks":["fix-git"]}`, `"tasks":["fix-git"]`+block+`}`, 1)
			_, err := Decode(strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), name+" must not be set for roadmapbench") {
				t.Fatalf("unrelated %s block accepted: %v", name, err)
			}
		})
	}
}

func TestRoadmapBenchVersionCatalogRemainsOptionalAndPinned(t *testing.T) {
	if _, err := DecodeVersions(strings.NewReader(validVersions)); err != nil {
		t.Fatalf("legacy catalog rejected: %v", err)
	}
	catalog := strings.TrimSuffix(validVersions, "}") + "," + roadmapBenchVersionSection + "}"
	versions, err := DecodeVersions(strings.NewReader(catalog))
	if err != nil {
		t.Fatal(err)
	}
	if versions.RoadmapBench.RepositoryURL != "https://huggingface.co/datasets/UnipatAI/RoadmapBench" || versions.RoadmapBench.Revision != "59184e779909300a5a0150b06b945d39da81a099" {
		t.Fatalf("RoadmapBench pins = %#v", versions.RoadmapBench)
	}
	for name, section := range map[string]string{
		"missing repository": `"roadmapbench":{"revision":"59184e779909300a5a0150b06b945d39da81a099"}`,
		"missing revision":   `"roadmapbench":{"repository_url":"https://huggingface.co/datasets/UnipatAI/RoadmapBench"}`,
		"moving revision":    strings.Replace(roadmapBenchVersionSection, "59184e779909300a5a0150b06b945d39da81a099", "main", 1),
		"short revision":     strings.Replace(roadmapBenchVersionSection, "59184e779909300a5a0150b06b945d39da81a099", "59184e7", 1),
		"insecure URL":       strings.Replace(roadmapBenchVersionSection, "https://", "http://", 1),
		"credential URL":     strings.Replace(roadmapBenchVersionSection, "https://", "https://key@", 1),
		"query URL":          strings.Replace(roadmapBenchVersionSection, "/RoadmapBench\"", "/RoadmapBench?token=secret\"", 1),
		"unknown field":      strings.Replace(roadmapBenchVersionSection, `"revision":`, `"image":"ignored","revision":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			input := strings.TrimSuffix(validVersions, "}") + "," + section + "}"
			if _, err := DecodeVersions(strings.NewReader(input)); err == nil {
				t.Fatal("invalid RoadmapBench pin was accepted")
			}
		})
	}
}

func TestLoadRoadmapBenchRequiresSelectedVersion(t *testing.T) {
	root := t.TempDir()
	versionsPath := filepath.Join(root, "versions.json")
	profilePath := filepath.Join(root, "profile.json")
	base := strings.Replace(validConfig, "../configs/versions.json", "versions.json", 1)
	profile := strings.Replace(base, `"type":"terminalbench2"`, `"type":"roadmapbench"`, 1)
	if err := os.WriteFile(versionsPath, []byte(validVersions), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(profilePath); err != nil {
		t.Fatalf("legacy profile rejected: %v", err)
	}
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(profilePath); err == nil || !strings.Contains(err.Error(), "roadmapbench.repository_url") {
		t.Fatalf("RoadmapBench accepted missing version pins: %v", err)
	}
	catalog := strings.TrimSuffix(validVersions, "}") + "," + roadmapBenchVersionSection + "}"
	if err := os.WriteFile(versionsPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(profilePath); err != nil {
		t.Fatalf("pinned RoadmapBench profile rejected: %v", err)
	}
}

func TestCheckedInRoadmapBenchProfileLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "profiles", "codex-roadmapbench-smoke1-openai.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Benchmark.Type != "roadmapbench" || cfg.Benchmark.Root != ".cache/roadmapbench" || len(cfg.Benchmark.Tasks) != 1 || cfg.Benchmark.Tasks[0] != "opt-3.0.0-roadmap" {
		t.Fatalf("RoadmapBench selection = %#v", cfg.Benchmark)
	}
	if cfg.OverridesFile != "" || cfg.Harness.Type != "codex" || cfg.Bridge.Type != "codex-ssh" || cfg.Runtime.Backend != "openai" {
		t.Fatalf("RoadmapBench profile = %#v", cfg)
	}
}

func TestCheckedInRoadmapBenchAllProfileLoads(t *testing.T) {
	profilePath := filepath.Join("..", "..", "profiles", "codex-roadmapbench-all115-openai.json")
	cfg, err := Load(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "codex-roadmapbench-all115-openai" || cfg.Benchmark.Type != "roadmapbench" || cfg.Benchmark.Root != ".cache/roadmapbench" {
		t.Fatalf("RoadmapBench full profile identity = %q, %#v", cfg.Name, cfg.Benchmark)
	}
	tasks := cfg.Benchmark.Tasks
	if len(tasks) != 115 {
		t.Fatalf("RoadmapBench task count = %d, want 115", len(tasks))
	}
	for i := 1; i < len(tasks); i++ {
		if tasks[i-1] >= tasks[i] {
			t.Fatalf("tasks must be unique and sorted: %q before %q", tasks[i-1], tasks[i])
		}
	}
	// SHA-256 of the sorted task directory names, each followed by a newline,
	// from dataset revision 59184e779909300a5a0150b06b945d39da81a099.
	const taskSetSHA256 = "7c395b8981f6fc8ebae8246f912b4cc4e66548b87777783018b2df726afd6111"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(tasks, "\n")+"\n"))); got != taskSetSHA256 {
		t.Fatalf("RoadmapBench task inventory SHA-256 = %s, want %s", got, taskSetSHA256)
	}
	if cfg.Harness.Type != "codex" || cfg.Harness.Mode != "agent" || cfg.Harness.Codex == nil || cfg.Bridge.Type != "codex-ssh" || cfg.Sandbox.Type != "docker" {
		t.Fatalf("RoadmapBench component selection = %#v, %#v, %#v", cfg.Harness, cfg.Bridge, cfg.Sandbox)
	}
	wantRoadmap := RoadmapBenchVersions{
		RepositoryURL: "https://huggingface.co/datasets/UnipatAI/RoadmapBench",
		Revision:      "59184e779909300a5a0150b06b945d39da81a099",
	}
	wantCodex := CodexVersions{Image: "docker.io/library/debian:12.12-slim", Version: "0.157.1"}
	if cfg.Versions.RoadmapBench != wantRoadmap || cfg.Versions.Codex != wantCodex {
		t.Fatalf("RoadmapBench/Codex pins = %#v, %#v", cfg.Versions.RoadmapBench, cfg.Versions.Codex)
	}
	if cfg.VersionsFile != "../configs/versions.json" || cfg.OverridesFile != "" || cfg.OutputDir != "runs" || cfg.Harness.Codex.Executable != "../.cache/codex/0.157.1/codex" {
		t.Fatalf("RoadmapBench profile paths = %#v", cfg)
	}
	wantExecutable, err := filepath.Abs(filepath.Join("..", "..", ".cache", "codex", "0.157.1", "codex"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Codex.ResolvedExecutable != wantExecutable {
		t.Fatalf("resolved Codex executable = %q, want %q", cfg.Harness.Codex.ResolvedExecutable, wantExecutable)
	}
	smoke, err := Load(filepath.Join("..", "..", "profiles", "codex-roadmapbench-smoke1-openai.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Name = smoke.Name
	cfg.Benchmark.Tasks = smoke.Benchmark.Tasks
	if !reflect.DeepEqual(cfg, smoke) {
		t.Fatal("full profile settings differ from smoke profile beyond name and task selection")
	}
}

func TestCheckedInRoadmapBenchFirst20ProfileLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "profiles", "codex-roadmapbench-qwen38-27b-xhigh-first20.json"))
	if err != nil {
		t.Fatal(err)
	}
	full, err := Load(filepath.Join("..", "..", "profiles", "codex-roadmapbench-all115-openai.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Benchmark.Tasks) != 115 || !reflect.DeepEqual(cfg.Benchmark.Tasks, full.Benchmark.Tasks[:20]) {
		t.Fatalf("first20 tasks = %v, want the first 20 tasks from the pinned full profile", cfg.Benchmark.Tasks)
	}
	if cfg.Name != "codex-roadmapbench-qwen38-27b-xhigh-first20" || cfg.Model.ID != "Qwen/Qwen3.8-27B" || cfg.Execution.Concurrency != 1 || cfg.OverridesFile != "" {
		t.Fatalf("first20 model/execution settings = %#v", cfg)
	}
	if cfg.Harness.Type != "codex" || cfg.Bridge.Type != "codex-ssh" || cfg.Harness.Codex == nil || cfg.Harness.Codex.ReasoningEffort != "xhigh" {
		t.Fatalf("first20 Codex settings = %#v, %#v", cfg.Harness, cfg.Bridge)
	}
	if cfg.Harness.Subagents.Enabled == nil || !*cfg.Harness.Subagents.Enabled || cfg.Harness.Subagents.MaxConcurrent != 3 {
		t.Fatalf("first20 native subagent settings = %#v", cfg.Harness.Subagents)
	}
	for _, instruction := range []string{
		"ultra execution strategy",
		"start with a brief plan",
		"delegate at least one substantive independent subtask early, before the main implementation",
		"investigation, implementation, or test review",
		"Actively parallelize independent work",
		"at most three active children",
		"bounded task and a verification target",
		"same sandbox and working tree",
		"mutually exclusive file ownership",
		"omit model, reasoning_effort, and agent_type",
		"inherits the parent's model, reasoning effort, and remote environment",
		"inspect and verify child results",
		"run the relevant tests yourself",
		"confirm the requested task is complete",
		"Execute ordinary reversible actions autonomously",
		"If you are a delegated child, stay within the assigned scope",
	} {
		if !strings.Contains(cfg.Harness.Codex.DeveloperInstructions, instruction) {
			t.Errorf("native developer instructions missing %q", instruction)
		}
	}
	// Endpoint, context window, pins, paths, and other defaults stay identical
	// to the complete profile; only this explicitly requested selection differs.
	cfg.Name = full.Name
	cfg.Benchmark.Tasks = full.Benchmark.Tasks
	cfg.Model.ID = full.Model.ID
	cfg.Harness.Codex.ReasoningEffort = full.Harness.Codex.ReasoningEffort
	cfg.Harness.Codex.DeveloperInstructions = full.Harness.Codex.DeveloperInstructions
	cfg.Harness.Subagents = full.Harness.Subagents
	if !reflect.DeepEqual(cfg, full) {
		t.Fatal("first20 profile differs from the full profile beyond its task, model, and delegation settings")
	}
}

func TestCheckedInRoadmapBenchQwen36FP8First20ProfileLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "profiles", "codex-roadmapbench-qwen36-35b-a3b-fp8-xhigh-first20.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Name != "codex-roadmapbench-qwen36-35b-a3b-fp8-xhigh-first20" || cfg.Model.ID != "Qwen/Qwen3.6-35B-A3B-FP8" {
		t.Fatalf("Qwen3.6 FP8 first20 identity = %q, %q", cfg.Name, cfg.Model.ID)
	}
	previous, err := Load(filepath.Join("..", "..", "profiles", "codex-roadmapbench-qwen38-27b-xhigh-first20.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Name = previous.Name
	cfg.Model.ID = previous.Model.ID
	if !reflect.DeepEqual(cfg, previous) {
		t.Fatal("Qwen3.6 FP8 first20 profile differs from Qwen3.8 beyond its name and model")
	}
}
