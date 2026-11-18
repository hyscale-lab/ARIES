package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/internal/procstat"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/sirupsen/logrus"
)

const cleanupRunID = "20261111T000000.000000000Z-test"

// cleanupFixture writes a profile whose output directory holds one run
// directory, with the runner record given, if any.
func cleanupFixture(t *testing.T, record *runnerRecord) string {
	t.Helper()
	output := t.TempDir()
	runDir := filepath.Join(output, cleanupRunID)
	if err := os.Mkdir(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if record != nil {
		content, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runDir, runnerRecordName), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return writeCommandProfile(t, output)
}

// fakeLeftovers is a backend that left the named containers.
type fakeLeftovers struct {
	found      []string
	fail       map[string]error
	closeErr   error
	asked      int
	runID      string
	outputRoot string
	removed    []string
	closed     bool
}

func (f *fakeLeftovers) wiring() Wiring {
	return Wiring{FindLeftovers: func(_ context.Context, _ config.Config, runID, outputRoot string, _ *logrus.Logger) ([]Leftover, func() error, error) {
		f.asked++
		f.runID, f.outputRoot = runID, outputRoot
		var leftovers []Leftover
		for _, name := range f.found {
			leftovers = append(leftovers, Leftover{Kind: "container", Name: name, Remove: func(context.Context) error {
				f.removed = append(f.removed, name)
				return f.fail[name]
			}})
		}
		return leftovers, func() error { f.closed = true; return f.closeErr }, nil
	}}
}

// cleanupDependencies reports this host as host, and every PID as alive with
// the given start time, or as gone.
func cleanupDependencies(wiring Wiring, host string, start uint64, alive bool) Dependencies {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return Dependencies{
		Wiring: wiring, Logger: logger,
		Hostname:     func() (string, error) { return host, nil },
		ProcessStart: func(int) (uint64, bool, error) { return start, alive, nil },
	}
}

func TestCleanupRefusesWhileTheRunnerRuns(t *testing.T) {
	profile := cleanupFixture(t, &runnerRecord{Host: "node", PID: 42, Start: 100})
	backend := &fakeLeftovers{found: []string{"a"}}
	err := Cleanup(context.Background(), profile, cleanupRunID, CleanupOptions{}, io.Discard, cleanupDependencies(backend.wiring(), "node", 100, true))
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err=%v", err)
	}
	if backend.asked != 0 || len(backend.removed) != 0 {
		t.Fatalf("asked=%d removed=%v", backend.asked, backend.removed)
	}
}

func TestCleanupRemovesWhatAnExitedRunnerLeft(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start uint64
		alive bool
	}{
		{name: "process gone", alive: false},
		{name: "PID reused", start: 200, alive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := cleanupFixture(t, &runnerRecord{Host: "node", PID: 42, Start: 100})
			backend := &fakeLeftovers{found: []string{"harness", "sandbox", "network"}}
			var out bytes.Buffer
			if err := Cleanup(context.Background(), profile, cleanupRunID, CleanupOptions{}, &out, cleanupDependencies(backend.wiring(), "node", tc.start, tc.alive)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(backend.removed, []string{"harness", "sandbox", "network"}) || backend.runID != cleanupRunID || !backend.closed {
				t.Fatalf("removed=%v run=%q closed=%v", backend.removed, backend.runID, backend.closed)
			}
			if filepath.Base(backend.outputRoot) != cleanupRunID || !filepath.IsAbs(backend.outputRoot) {
				t.Fatalf("run directory=%q", backend.outputRoot)
			}
			if want := "removed container harness\nremoved container sandbox\nremoved container network\n"; out.String() != want {
				t.Fatalf("out=%q", out.String())
			}
		})
	}
}

func TestCleanupNeedsForceWhenItCannotTell(t *testing.T) {
	for _, tc := range []struct {
		name    string
		record  *runnerRecord
		garbled bool
		want    string
	}{
		{name: "no record", want: runnerRecordName},
		{name: "unreadable record", garbled: true, want: "decode " + runnerRecordName},
		{name: "another host", record: &runnerRecord{Host: "elsewhere", PID: 42, Start: 100}, want: `host "elsewhere"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := cleanupFixture(t, tc.record)
			if tc.garbled {
				cfg, err := config.Load(profile)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cfg.OutputDir, cleanupRunID, runnerRecordName), []byte(`{"host":`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			backend := &fakeLeftovers{found: []string{"a"}}
			dependencies := cleanupDependencies(backend.wiring(), "node", 100, true)
			err := Cleanup(context.Background(), profile, cleanupRunID, CleanupOptions{}, io.Discard, dependencies)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--force") || backend.asked != 0 {
				t.Fatalf("err=%v asked=%d", err, backend.asked)
			}
			if err := Cleanup(context.Background(), profile, cleanupRunID, CleanupOptions{Force: true}, io.Discard, dependencies); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(backend.removed, []string{"a"}) {
				t.Fatalf("removed=%v", backend.removed)
			}
		})
	}
}

func TestCleanupDryRunRemovesNothing(t *testing.T) {
	profile := cleanupFixture(t, &runnerRecord{Host: "node", PID: 42, Start: 100})
	backend := &fakeLeftovers{found: []string{"a", "b"}}
	var out bytes.Buffer
	if err := Cleanup(context.Background(), profile, cleanupRunID, CleanupOptions{DryRun: true}, &out, cleanupDependencies(backend.wiring(), "node", 0, false)); err != nil {
		t.Fatal(err)
	}
	if len(backend.removed) != 0 || out.String() != "would remove container a\nwould remove container b\n" {
		t.Fatalf("removed=%v out=%q", backend.removed, out.String())
	}
}

func TestCleanupTriesEveryLeftoverAndReportsFailures(t *testing.T) {
	profile := cleanupFixture(t, &runnerRecord{Host: "node", PID: 42, Start: 100})
	stuck := errors.New("stuck")
	backend := &fakeLeftovers{found: []string{"a", "b", "c"}, fail: map[string]error{"b": stuck}}
	var out bytes.Buffer
	err := Cleanup(context.Background(), profile, cleanupRunID, CleanupOptions{}, &out, cleanupDependencies(backend.wiring(), "node", 0, false))
	if !errors.Is(err, stuck) || !strings.Contains(err.Error(), "1 of 3") {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(backend.removed, []string{"a", "b", "c"}) || !strings.Contains(out.String(), "failed to remove container b\n") {
		t.Fatalf("removed=%v out=%q", backend.removed, out.String())
	}
}

func TestCleanupReportsARunThatLeftNothing(t *testing.T) {
	profile := cleanupFixture(t, &runnerRecord{Host: "node", PID: 42, Start: 100})
	closeErr := errors.New("close")
	backend := &fakeLeftovers{closeErr: closeErr}
	var out bytes.Buffer
	err := Cleanup(context.Background(), profile, cleanupRunID, CleanupOptions{}, &out, cleanupDependencies(backend.wiring(), "node", 0, false))
	if !errors.Is(err, closeErr) || out.String() != "run "+cleanupRunID+" left nothing to remove\n" {
		t.Fatalf("err=%v out=%q", err, out.String())
	}
}

func TestCleanupTakesOnlyARunDirectoryName(t *testing.T) {
	profile := cleanupFixture(t, nil)
	for _, runID := range []string{"", ".", "..", "../" + cleanupRunID, "a/b", `a\b`} {
		backend := &fakeLeftovers{found: []string{"a"}}
		err := Cleanup(context.Background(), profile, runID, CleanupOptions{Force: true}, io.Discard, cleanupDependencies(backend.wiring(), "node", 0, false))
		if err == nil || backend.asked != 0 {
			t.Fatalf("run ID %q: err=%v asked=%d", runID, err, backend.asked)
		}
	}
}

func TestRunnerRecordIdentifiesThisProcess(t *testing.T) {
	dir := t.TempDir()
	if err := writeRunnerRecord(dir, os.Hostname, procstat.StartTime); err != nil {
		t.Fatal(err)
	}
	record, err := readRunnerRecord(dir)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	started, alive, err := procstat.StartTime(os.Getpid())
	if err != nil || !alive || record.PID != os.Getpid() || record.Host != host || record.Start != started {
		t.Fatalf("record=%+v start=%d alive=%v err=%v", record, started, alive, err)
	}
	info, err := os.Stat(filepath.Join(dir, runnerRecordName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("info=%v err=%v", info, err)
	}
	if err := refuseLiveRunner(dir, os.Hostname, procstat.StartTime); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("a live Runner was not refused: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the record's temporary file was left: %v %v", entries, err)
	}
}
