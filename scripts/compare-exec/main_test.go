package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDistributionsKeepSmallSamplesSeparateFromP95(t *testing.T) {
	small := stats([]float64{9, 1, 3, 5})
	if small.N != 4 || small.Min != 1 || small.Median != 4 || small.Max != 9 || small.P95 != nil {
		t.Fatalf("small distribution=%+v", small)
	}
	values := make([]float64, 20)
	for i := range values {
		values[i] = float64(20 - i)
	}
	large := stats(values)
	if large.Median != 10.5 || large.P95 == nil || *large.P95 != 19 {
		t.Fatalf("large distribution=%+v", large)
	}
}
func TestPartialComparisonDoesNotInventNativeReference(t *testing.T) {
	dir := t.TempDir()
	if err := writeSummary(dir, []*environment{{Name: "not-created"}}, nil); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]map[string]any
	if err = json.Unmarshal(content, &summary); err != nil {
		t.Fatal(err)
	}
	if summary["not-created"]["native_spawn_capture_wait"] != nil {
		t.Fatalf("missing native measurement became zero: %s", content)
	}
}
