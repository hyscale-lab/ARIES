package config

import (
	"os"
	"path/filepath"
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
