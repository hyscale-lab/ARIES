package benchmark

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestSWEbenchProRequiresBothPinnedRevisions(t *testing.T) {
	cfg := config.Config{}
	cfg.Benchmark.Root = t.TempDir()
	cfg.Versions.SWEbenchPro.DatasetRevision = strings.Repeat("a", 40)
	if _, err := NewSWEbenchPro(cfg, t.TempDir(), []string{"task-1"}, nil, nil); err == nil {
		t.Fatal("missing evaluator revision accepted")
	}
	cfg.Versions.SWEbenchPro.EvaluatorRevision = strings.Repeat("b", 40)
	if _, err := NewSWEbenchPro(cfg, t.TempDir(), []string{"task-1"}, []string{"task-1-001"}, nil); err != nil {
		t.Fatal(err)
	}
	cfg.Versions.SWEbenchPro.DatasetRevision = ""
	if _, err := NewSWEbenchPro(cfg, t.TempDir(), []string{"task-1"}, nil, nil); err == nil {
		t.Fatal("missing dataset revision accepted")
	}
}
