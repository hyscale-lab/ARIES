package benchmark

import (
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

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
