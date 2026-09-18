package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const arrivalsFixture = `{"trace_id":"t","base_rate_per_min":1.0,"arrivals":[
 {"t":60.0,"traj":"b"},{"t":120.0,"traj":"a"},{"t":300.0,"traj":"b"}]}`

func writeArrivals(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadArrivalsScalesAndOrdersByTraceOccurrence(t *testing.T) {
	path := writeArrivals(t, arrivalsFixture)
	got, err := LoadArrivals(path, 2.0, []string{"a", "b", "b"})
	if err != nil {
		t.Fatal(err)
	}
	// Rate 2/min halves every base-rate offset; b's two occurrences take b's
	// two arrivals in trace order.
	want := []Arrival{{"b", 30 * time.Second}, {"a", 60 * time.Second}, {"b", 150 * time.Second}}
	if len(got) != len(want) {
		t.Fatalf("arrivals = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arrivals[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestLoadArrivalsRefusesMissingTasksAndBadRates(t *testing.T) {
	path := writeArrivals(t, arrivalsFixture)
	if _, err := LoadArrivals(path, 1.0, []string{"a", "a"}); err == nil || !strings.Contains(err.Error(), `task "a"`) {
		t.Fatalf("second occurrence of a accepted: %v", err)
	}
	if _, err := LoadArrivals(path, 1.0, []string{"zzz"}); err == nil {
		t.Fatal("unknown task accepted")
	}
	for _, rate := range []float64{0, -1} {
		if _, err := LoadArrivals(path, rate, []string{"a"}); err == nil {
			t.Fatalf("rate %g accepted", rate)
		}
	}
	if _, err := LoadArrivals(writeArrivals(t, `{"base_rate_per_min":0,"arrivals":[{"t":1,"traj":"a"}]}`), 1.0, []string{"a"}); err == nil {
		t.Fatal("zero base rate accepted")
	}
}

func TestDecodeArrivalsFieldsMustPairAndExcludeLoop(t *testing.T) {
	with := func(execution string) string {
		return strings.Replace(validConfig, `"benchmark":`, `"execution":`+execution+`,"benchmark":`, 1)
	}
	cfg, err := Decode(strings.NewReader(with(`{"concurrency":89,"arrivals_file":"trace.json","arrival_rate_per_min":0.5}`)))
	if err != nil || cfg.Execution.ArrivalsFile != "trace.json" || cfg.Execution.ArrivalRatePerMin != 0.5 {
		t.Fatalf("execution=%#v err=%v", cfg.Execution, err)
	}
	for _, bad := range []string{
		`{"concurrency":1,"arrivals_file":"trace.json"}`,
		`{"concurrency":1,"arrival_rate_per_min":0.5}`,
		`{"concurrency":1,"arrivals_file":"trace.json","arrival_rate_per_min":-1}`,
		`{"concurrency":1,"arrivals_file":"trace.json","arrival_rate_per_min":0.5,"loop_duration":"1m"}`,
	} {
		if _, err := Decode(strings.NewReader(with(bad))); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
