package benchmark

import (
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestReplaySWEAtlasNeedsNoJudgeKey(t *testing.T) {
	cfg := config.Config{Replay: &config.ReplayConfig{Source: "source-run/task-001"}}
	cfg.Benchmark.Root = t.TempDir()
	cfg.Versions.SWEAtlas.Revision = "revision"
	benchmark, err := NewSWEAtlas(cfg, t.TempDir(), []string{"task"}, nil, func(string) ([]byte, bool) {
		t.Fatal("replay looked up a judge credential")
		return nil, false
	})
	if err != nil || benchmark == nil {
		t.Fatalf("replay benchmark = %v, error = %v", benchmark, err)
	}
}

func TestSWEAtlasDisabledJudgeNeedsNoCredentials(t *testing.T) {
	cfg := config.Config{}
	cfg.Benchmark.Root = t.TempDir()
	cfg.Versions.SWEAtlas.Revision = "revision"
	enabled := false
	cfg.Benchmark.Judge = &config.JudgeConfig{Enabled: &enabled}
	_, err := NewSWEAtlas(cfg, t.TempDir(), []string{"task-1"}, []string{"task-1-001"}, func(string) ([]byte, bool) {
		t.Fatal("disabled judge looked up credentials")
		return nil, false
	})
	if err != nil {
		t.Fatal(err)
	}
}
