package replay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const timingFixtureStart = "2026-11-05T12:00:00Z"

func writeTimingAudit(t *testing.T, records ...map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tool-calls.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateTimingFileMode)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestReadTimingPairsSourceSequenceAndSignedDrift(t *testing.T) {
	const firstSourceSequence uint64 = 1
	const secondSourceSequence uint64 = 2
	const thirdSourceSequence uint64 = 3
	const sourceGap = 20 * time.Millisecond
	const lateDrift = 3 * time.Millisecond
	const earlyDrift = -2 * time.Millisecond
	start, err := time.Parse(time.RFC3339Nano, timingFixtureStart)
	if err != nil {
		t.Fatal(err)
	}
	plan := Plan{Calls: []Call{
		{Sequence: firstSourceSequence},
		{Sequence: secondSourceSequence, StartOffset: sourceGap},
		{Sequence: thirdSourceSequence, StartOffset: time.Duration(secondSourceSequence) * sourceGap},
	}}
	// Audit records are in completion order, not source start order.
	path := writeTimingAudit(t,
		map[string]any{"replay_source_sequence": thirdSourceSequence, "started_at": start.Add(time.Duration(secondSourceSequence)*sourceGap + earlyDrift).Format(time.RFC3339Nano), "tool_call_id": "shared-tool"},
		map[string]any{"replay_source_sequence": firstSourceSequence, "started_at": start.Format(time.RFC3339Nano), "tool_call_id": "shared-tool"},
		map[string]any{"replay_source_sequence": secondSourceSequence, "started_at": start.Add(sourceGap + lateDrift).Format(time.RFC3339Nano), "tool_call_id": "shared-tool"},
	)
	want := core.ReplayTiming{Status: "available", Calls: []core.ReplayCallTiming{
		{SourceSequence: firstSourceSequence, DriftNS: 0},
		{SourceSequence: secondSourceSequence, DriftNS: lateDrift.Nanoseconds()},
		{SourceSequence: thirdSourceSequence, DriftNS: earlyDrift.Nanoseconds()},
	}}
	if got := ReadTiming(path, plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("signed timing drift = %+v, want %+v", got, want)
	}
}

func TestReadTimingRejectsUnsafeAudit(t *testing.T) {
	const publicAuditMode = 0o644
	for name, change := range map[string]func(*testing.T, string) string{
		"public file": func(t *testing.T, path string) string {
			if err := os.Chmod(path, publicAuditMode); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"symlink": func(t *testing.T, path string) string {
			link := filepath.Join(filepath.Dir(path), "linked-audit.jsonl")
			if err := os.Symlink(path, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"oversize": func(t *testing.T, path string) string {
			if err := os.Truncate(path, maxTimingAuditBytes+1); err != nil {
				t.Fatal(err)
			}
			return path
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := change(t, writeTimingAudit(t))
			got := ReadTiming(path, Plan{})
			if got.Status != "unavailable" || got.Reason == "" || len(got.Calls) != 0 {
				t.Fatalf("unsafe audit accepted: %+v", got)
			}
		})
	}
}

func TestReadTimingMarksMissingAndDuplicateCorrelationUnavailable(t *testing.T) {
	const firstSourceSequence uint64 = 1
	const secondSourceSequence uint64 = 2
	start := timingFixtureStart
	plan := Plan{Calls: []Call{{Sequence: firstSourceSequence}, {Sequence: secondSourceSequence}}}
	for name, records := range map[string][]map[string]any{
		"missing": {
			{"replay_source_sequence": firstSourceSequence, "started_at": start},
			{"started_at": start},
		},
		"duplicate": {
			{"replay_source_sequence": firstSourceSequence, "started_at": start},
			{"replay_source_sequence": firstSourceSequence, "started_at": start},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := ReadTiming(writeTimingAudit(t, records...), plan)
			if got.Status != "unavailable" || got.Reason == "" || len(got.Calls) != 0 {
				t.Fatalf("ambiguous timing was guessed: %+v", got)
			}
		})
	}
}
