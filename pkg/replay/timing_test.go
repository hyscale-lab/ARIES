package replay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
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

func TestReadResponsesLogsLegacyAndBinaryDriftWithoutResultText(t *testing.T) {
	const firstSequence uint64 = 1
	const nextSequence uint64 = firstSequence + 1
	const expectedHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const actualHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const sourceText = "source git hash aaa"
	const replayText = "replay git hash bbb"
	// This byte cannot start a valid UTF-8 sequence.
	const invalidUTF8Byte byte = 0xff
	binary := []byte{invalidUTF8Byte}
	for _, sourceDebug := range []bool{true, false} {
		var logs bytes.Buffer
		logger := logrus.New()
		logger.SetOutput(&logs)
		path := writeTimingAudit(t,
			map[string]any{"replay_source_sequence": firstSequence, "operation_class": "exec", "response_sha256": actualHash, "response_debug": true, "response_stdout": replayText, "response_stderr_base64": base64.StdEncoding.EncodeToString(binary)},
			map[string]any{"replay_source_sequence": nextSequence, "operation_class": "stat", "response_sha256": actualHash},
		)
		plan := Plan{Calls: []Call{{Sequence: firstSequence, Operation: "Exec", ResponseSHA256: expectedHash, ResponseDebug: sourceDebug, ResponseStdout: []byte(sourceText)}, {Sequence: nextSequence, Operation: "Stat", ResponseSHA256: actualHash}}}
		got := ReadResponses(path, plan, logger)
		if got.Status != "available" || len(got.Calls) != len(plan.Calls) || got.Calls[0].Status != "drifted" || got.Calls[1].Status != "matched" {
			t.Fatalf("response summary = %+v", got)
		}
		if !strings.Contains(logs.String(), expectedHash) || !strings.Contains(logs.String(), actualHash) || !strings.Contains(logs.String(), replayText) || !strings.Contains(logs.String(), "base64: "+base64.StdEncoding.EncodeToString(binary)) {
			t.Fatalf("missing drift hashes or replay output: %s", logs.String())
		}
		if sourceDebug && !strings.Contains(logs.String(), sourceText) || !sourceDebug && !strings.Contains(logs.String(), "source output unavailable") {
			t.Fatalf("wrong source output log: %s", logs.String())
		}
		encoded, err := json.Marshal(got)
		if err != nil || strings.Contains(string(encoded), sourceText) || strings.Contains(string(encoded), replayText) {
			t.Fatalf("response text entered result: %s, %v", encoded, err)
		}
	}
}

func TestReadResponsesLogsFinalizedDriftEvenWithMissingCalls(t *testing.T) {
	const firstSequence uint64 = 1
	const nextSequence uint64 = firstSequence + 1
	const sourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const replayHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var logs bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&logs)
	path := writeTimingAudit(t, map[string]any{"replay_source_sequence": firstSequence, "operation_class": "stat", "response_sha256": replayHash})
	plan := Plan{Calls: []Call{{Sequence: firstSequence, Operation: "Stat", ResponseSHA256: sourceHash}, {Sequence: nextSequence, Operation: "Stat", ResponseSHA256: sourceHash}}}
	got := ReadResponses(path, plan, logger)
	if got.Status != "unavailable" || len(got.Calls) != 0 || !strings.Contains(logs.String(), sourceHash) || !strings.Contains(logs.String(), replayHash) || strings.Contains(logs.String(), "stdout") {
		t.Fatalf("partial strict failure lost drift log or invented file text: %+v; log: %s", got, logs.String())
	}
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
	want := core.ReplayTiming{Status: "available", Basis: "occurrence_first_start", Calls: []core.ReplayCallTiming{
		{SourceSequence: firstSourceSequence, DriftNS: 0},
		{SourceSequence: secondSourceSequence, DriftNS: lateDrift.Nanoseconds()},
		{SourceSequence: thirdSourceSequence, DriftNS: earlyDrift.Nanoseconds()},
	}}
	if got := ReadTiming(path, plan); !reflect.DeepEqual(got, want) {
		t.Fatalf("signed timing drift = %+v, want %+v", got, want)
	}
}

func TestReadTimingKeepsCrossOccurrenceDisplacement(t *testing.T) {
	const firstSequence uint64 = 1
	const nextSequence = firstSequence + 1
	const sourceGap = time.Second
	const callGap = sourceGap / 2
	const firstLateness = 3 * time.Millisecond
	const laterLateness = 40 * time.Millisecond
	origin, err := time.Parse(time.RFC3339Nano, timingFixtureStart)
	if err != nil {
		t.Fatal(err)
	}
	for _, occurrence := range []struct {
		taskID        string
		offset, drift time.Duration
	}{
		{taskID: "a-001", drift: firstLateness},
		{taskID: "a-002", offset: sourceGap, drift: laterLateness},
	} {
		t.Run(occurrence.taskID, func(t *testing.T) {
			plan := Plan{SourceRunID: "source-run", SourceTaskID: occurrence.taskID, ReplayAt: &origin, Calls: []Call{
				{Sequence: firstSequence, StartOffset: occurrence.offset},
				{Sequence: nextSequence, StartOffset: occurrence.offset + callGap},
			}}
			// A run-local task string can match the source in a different run.
			path := writeTimingAudit(t,
				map[string]any{"run_id": "new-run", "task_id": occurrence.taskID, "replay_source_sequence": nextSequence, "started_at": origin.Add(occurrence.offset + callGap + occurrence.drift).Format(time.RFC3339Nano)},
				map[string]any{"run_id": "new-run", "task_id": occurrence.taskID, "replay_source_sequence": firstSequence, "started_at": origin.Add(occurrence.offset + occurrence.drift).Format(time.RFC3339Nano)},
			)
			got := ReadTiming(path, plan)
			if got.Status != "available" || got.Basis != "shared_replay_origin" || len(got.Calls) != len(plan.Calls) {
				t.Fatalf("timing unavailable: %+v", got)
			}
			for index, call := range got.Calls {
				if call.SourceSequence != plan.Calls[index].Sequence || call.DriftNS != occurrence.drift.Nanoseconds() {
					t.Fatalf("occurrence displacement was hidden: %+v, want %v", got, occurrence.drift)
				}
			}
		})
	}
}

func TestReadTimingSharedOriginKeepsEarlyStartsAndRejectsOverflow(t *testing.T) {
	const sequence uint64 = 1
	const beforeOrigin = -2 * time.Nanosecond
	origin, err := time.Parse(time.RFC3339Nano, timingFixtureStart)
	if err != nil {
		t.Fatal(err)
	}
	path := writeTimingAudit(t, map[string]any{"replay_source_sequence": sequence, "started_at": origin.Add(beforeOrigin).Format(time.RFC3339Nano)})
	plan := Plan{ReplayAt: &origin, Calls: []Call{{Sequence: sequence}}}
	got := ReadTiming(path, plan)
	if got.Status != "available" || len(got.Calls) != len(plan.Calls) || got.Calls[0].DriftNS != beforeOrigin.Nanoseconds() {
		t.Fatalf("early bridge start lost signed drift: %+v", got)
	}
	plan.Calls[0].StartOffset = time.Duration(math.MaxInt64)
	if got := ReadTiming(path, plan); got.Status != "unavailable" || len(got.Calls) != 0 || !strings.Contains(got.Reason, "not representable") {
		t.Fatalf("shared drift overflow was accepted: %+v", got)
	}
	origin = time.Time{}
	if got := ReadTiming(path, plan); got.Status != "unavailable" || len(got.Calls) != 0 || !strings.Contains(got.Reason, "not representable") {
		t.Fatalf("shared offset overflow was accepted: %+v", got)
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
