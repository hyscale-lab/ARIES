package config

import (
	"strings"
	"testing"
)

func TestDecodeWarmupTakesDefaultsAndRejectsBadValues(t *testing.T) {
	with := func(execution string) string {
		return strings.Replace(validConfig, `"benchmark":`, `"execution":`+execution+`,"benchmark":`, 1)
	}
	cfg, err := Decode(strings.NewReader(with(`{"concurrency":1,"parallel_start":true,"warmup":{}}`)))
	if err != nil {
		t.Fatal(err)
	}
	want := WarmupConfig{Instruction: DefaultWarmupInstruction, Stable: DefaultWarmupStable, Tolerance: DefaultWarmupTolerance, Max: DefaultWarmupMax}
	if !cfg.Execution.ParallelStart || cfg.Execution.Warmup == nil || *cfg.Execution.Warmup != want {
		t.Fatalf("execution = %#v, warm-up = %#v", cfg.Execution, cfg.Execution.Warmup)
	}
	cfg, err = Decode(strings.NewReader(with(`{"concurrency":1}`)))
	if err != nil || cfg.Execution.Warmup != nil || cfg.Execution.ParallelStart {
		t.Fatalf("execution = %#v, err = %v", cfg.Execution, err)
	}
	for _, bad := range []string{
		`{"concurrency":1,"warmup":{"instruction":"   "}}`,
		`{"concurrency":1,"warmup":{"stable":5,"max":3}}`,
		`{"concurrency":1,"warmup":{"stable":-1}}`,
		`{"concurrency":1,"warmup":{"tolerance":-0.1}}`,
	} {
		if _, err := Decode(strings.NewReader(with(bad))); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
