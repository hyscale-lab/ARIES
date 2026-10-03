package benchmark

import (
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestTerminalBenchPreservesVerifierFloorAndOccurrenceValidation(t *testing.T) {
	cfg := config.Config{}
	cfg.Benchmark.Root = t.TempDir()
	cfg.Versions.TerminalBench2.Revision = "revision"
	floor := -time.Second
	cfg.Overrides.VerifierTimeoutFloor = &floor
	_, err := NewTerminalBench(cfg, t.TempDir(), []string{"fix-git"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "verifier timeout floor") {
		t.Fatalf("negative verifier floor error = %v", err)
	}
	floor = time.Minute
	if _, err := NewTerminalBench(cfg, t.TempDir(), []string{"fix-git"}, []string{"fix-git-001"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = NewTerminalBench(cfg, t.TempDir(), []string{"fix-git"}, []string{"other-task-001"}, nil)
	if err == nil || !strings.Contains(err.Error(), "execution task ID") {
		t.Fatalf("unrelated occurrence ID error = %v", err)
	}
	if _, err := NewTerminalBench(cfg, t.TempDir(), []string{"fix-git", "hello-world"}, nil, nil); err != nil {
		t.Fatalf("preparation task selection: %v", err)
	}
}
