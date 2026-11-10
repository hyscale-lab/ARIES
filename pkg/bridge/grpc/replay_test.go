package grpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

const (
	replayRunID      = "source-run"
	replayTaskID     = "task-001"
	replayLogicalID  = "task"
	replayImage      = "task-image:tag"
	replayBenchmark  = "terminalbench2"
	replayRevision   = "pinned-revision"
	replaySandboxID  = "source-sandbox"
	replayOutcome    = "recorded outcome"
	replaySourceTime = "2026-11-05T12:00:00Z"
)

var replayTask = core.Task{ID: replayLogicalID, Environment: core.Environment{Image: replayImage}}
var replayRevisions = map[string]string{"revision": replayRevision}

func replayRecord(started, completed time.Time, sequence uint64) toolCallRecord {
	digest := sha256.Sum256([]byte(replayOutcome))
	return toolCallRecord{ToolCallRecord: bridgekit.ToolCallRecord{
		SandboxID: replaySandboxID, RunID: replayRunID, TaskID: replayTaskID,
		Sequence: sequence, StartedAt: bridgekit.FormatTime(started), Timestamp: bridgekit.FormatTime(completed),
		OperationClass: kindFileStat, Path: "/app/file", Status: "completed", StdinEncoding: "utf-8",
	}, ResponseSHA256: hex.EncodeToString(digest[:]), ResponseBytes: int64(len(replayOutcome)), GRPCCode: "OK"}
}

func writeReplaySource(t *testing.T, records []toolCallRecord, completed time.Time, evaluation core.Evaluation) string {
	t.Helper()
	run := t.TempDir()
	if err := os.Chmod(run, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(run, replayTaskID)
	bridgeDir := filepath.Join(source, "bridge")
	if err := os.MkdirAll(bridgeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var log []byte
	for _, record := range records {
		line, err := bridgekit.MarshalJSONLine(record)
		if err != nil {
			t.Fatal(err)
		}
		log = append(log, line...)
	}
	if err := os.WriteFile(filepath.Join(bridgeDir, "tool-calls.jsonl"), log, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(log)
	metadata := map[string]any{
		"schema_version": 1, "run_id": replayRunID, "replay_ready": true, "SandboxID": replaySandboxID,
		"benchmark_type": replayBenchmark, "task_id": replayLogicalID, "image": replayImage,
		"benchmark_revisions": replayRevisions, "harness_completed_at": bridgekit.FormatTime(completed),
		"tool_calls_bytes": len(log), "tool_calls_sha256": hex.EncodeToString(digest[:]),
	}
	writeReplayJSON(t, filepath.Join(bridgeDir, "runtime.json"), metadata)
	writeReplayJSON(t, filepath.Join(run, "run-result.json"), core.RunResult{
		RunID: replayRunID, Tasks: []core.TaskResult{{TaskID: replayTaskID, Evaluation: evaluation}},
	})
	return source
}

func writeReplayJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReplaySourceOrdersCallsAndPreservesEvaluation(t *testing.T) {
	first, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const startGap = 3 * time.Second
	const callLength = 2 * time.Second
	const lastCompletionLag = time.Second
	const finalWait = 4 * time.Second
	late := replayRecord(first.Add(startGap), first.Add(startGap+callLength), 1)
	early := replayRecord(first, first.Add(startGap+callLength+lastCompletionLag), 2)
	early.OperationClass = kindExec
	early.Command = "printf ok"
	early.CommandHash = bridgekit.CommandHash(early.Command)
	early.Path = bridgekit.RemoteShellPath
	early.Workdir = testWorkdir
	early.Argv = []string{bridgekit.RemoteShellPath, "-c", early.Command}
	const sourceScore = 0.5
	const sourceReward = 1.0
	evaluation := core.Evaluation{Status: core.StatusSucceeded, Score: sourceScore, Reward: sourceReward}
	source := writeReplaySource(t, []toolCallRecord{late, early}, first.Add(startGap+callLength+lastCompletionLag+finalWait), evaluation)

	plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceRunID != replayRunID || plan.SourceTaskID != replayTaskID || plan.Evaluation.Status != evaluation.Status || plan.Evaluation.Score != sourceScore || plan.Evaluation.Reward != sourceReward {
		t.Fatalf("source evaluation and identity = %+v", plan)
	}
	if len(plan.Calls) != 2 || plan.Calls[0].Operation != "Exec" || plan.Calls[0].Sequence != early.Sequence || plan.Calls[0].StartOffset != 0 || plan.Calls[0].Script != early.Command || plan.Calls[1].Operation != "Stat" || plan.Calls[1].Sequence != late.Sequence || plan.Calls[1].StartOffset != startGap || plan.FinalWait != finalWait {
		t.Fatalf("call plan = %+v", plan)
	}
}

func TestLoadReplaySourceKeepsExecWorkdir(t *testing.T) {
	at, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	record := replayRecord(at, at, 1)
	record.OperationClass, record.Command = kindExec, "touch relative-file"
	record.CommandHash, record.Path, record.Workdir = bridgekit.CommandHash(record.Command), bridgekit.RemoteShellPath, testWorkdir
	record.Argv = []string{bridgekit.RemoteShellPath, "-c", record.Command}
	source := writeReplaySource(t, []toolCallRecord{record}, at, core.Evaluation{Status: core.StatusSucceeded})
	for _, workdir := range []string{"", testWorkdir, "/other"} {
		t.Run("workdir="+workdir, func(t *testing.T) {
			task := replayTask
			task.Environment.Workdir = workdir
			plan, err := LoadReplaySource(source, task, replayBenchmark, replayRevisions)
			if workdir != "" && workdir != testWorkdir {
				if err == nil || len(plan.Calls) != 0 {
					t.Fatalf("changed workdir accepted: %+v, %v", plan, err)
				}
				return
			}
			if err != nil || len(plan.Calls) != 1 {
				t.Fatalf("same workdir source: %+v, %v", plan, err)
			}
			data, err := json.Marshal(plan.Calls[0])
			if err != nil {
				t.Fatal(err)
			}
			var call struct {
				Workdir string `json:"workdir"`
			}
			if err := json.Unmarshal(data, &call); err != nil || call.Workdir != testWorkdir {
				t.Fatalf("recorded workdir lost: %+v, %v", call, err)
			}
		})
	}
}

func TestLoadReplaySourceKeepsExecExitAndOptionalOutput(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const sourceExitCode = 7
	const sourceStdout = "source git hash aaa"
	record := replayRecord(start, start.Add(time.Second), 1)
	record.OperationClass, record.Command = kindExec, "git rev-parse HEAD"
	record.CommandHash, record.Path, record.Workdir = bridgekit.CommandHash(record.Command), bridgekit.RemoteShellPath, testWorkdir
	record.Argv = []string{bridgekit.RemoteShellPath, "-c", record.Command}
	record.ExitCode = sourceExitCode
	// This byte cannot start a valid UTF-8 sequence.
	const invalidUTF8Byte byte = 0xff
	record.setDebugResponse([]byte(sourceStdout), []byte{invalidUTF8Byte})
	for _, debug := range []bool{true, false} {
		copy := record
		if !debug {
			copy.ResponseDebug = false
			copy.ResponseStdout, copy.ResponseStderr = "", ""
			copy.ResponseStderrBase64 = nil
		}
		source := writeReplaySource(t, []toolCallRecord{copy}, start.Add(2*time.Second), core.Evaluation{Status: core.StatusSucceeded})
		plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
		if err != nil {
			t.Fatal(err)
		}
		call := plan.Calls[0]
		if call.ExitCode != sourceExitCode || call.ResponseDebug != debug {
			t.Fatalf("Exec exit and debug availability = %+v", call)
		}
		if debug && (string(call.ResponseStdout) != sourceStdout || !bytes.Equal(call.ResponseStderr, []byte{invalidUTF8Byte})) {
			t.Fatalf("Exec text and binary source output = %+v", call)
		}
	}
}

func TestLoadReplaySourceRejectsExecExitOutsideBridgeRange(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	record := replayRecord(start, start.Add(time.Second), 1)
	record.OperationClass, record.Command = kindExec, "git rev-parse HEAD"
	record.CommandHash, record.Path, record.Workdir = bridgekit.CommandHash(record.Command), bridgekit.RemoteShellPath, testWorkdir
	record.Argv = []string{bridgekit.RemoteShellPath, "-c", record.Command}
	for _, invalidExitCode := range []int{refusedExitCode, int(^uint8(0)) + 1} {
		record.ExitCode = invalidExitCode
		source := writeReplaySource(t, []toolCallRecord{record}, start.Add(2*time.Second), core.Evaluation{Status: core.StatusSucceeded})
		if plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions); err == nil || len(plan.Calls) != 0 {
			t.Fatalf("invalid source Exec exit %d accepted: %+v, %v", invalidExitCode, plan, err)
		}
	}
	for name, change := range map[string]func(map[string]any){
		"missing": func(fields map[string]any) { delete(fields, "exit_code") },
		"null":    func(fields map[string]any) { fields["exit_code"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			record.ExitCode = 0
			source := writeReplaySource(t, []toolCallRecord{record}, start.Add(2*time.Second), core.Evaluation{Status: core.StatusSucceeded})
			rewriteReplayRecord(t, source, change)
			if plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions); err == nil || len(plan.Calls) != 0 {
				t.Fatalf("missing source Exec exit evidence accepted: %+v, %v", plan, err)
			}
		})
	}
}

func TestLoadReplaySourceRejectsAnyBadCall(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const earlierCalls = 10
	const callGap = time.Second
	const callDuration = time.Millisecond
	for name, change := range map[string]func(*toolCallRecord){
		"missing write body": func(record *toolCallRecord) {
			record.OperationClass, record.Path, record.StdinBytes = kindFileWrite, "/app/file", 1
		},
		"omitted write body": func(record *toolCallRecord) {
			record.OperationClass, record.Path, record.StdinEncoding, record.ReplayError = kindFileWrite, "/app/file", "binary-omitted", "write-content-omitted"
		},
		"rejected call":              func(record *toolCallRecord) { record.Status = "rejected" },
		"canceled call":              func(record *toolCallRecord) { record.Status = "canceled" },
		"replay error":               func(record *toolCallRecord) { record.ReplayError = "response-send-failed" },
		"failed file with OK status": func(record *toolCallRecord) { record.Status = "failed" },
		"missing fingerprint":        func(record *toolCallRecord) { record.ResponseSHA256 = "" },
		"missing outcome size":       func(record *toolCallRecord) { record.ResponseBytes = 0 },
		"invalid gRPC status":        func(record *toolCallRecord) { record.GRPCCode = "not-a-code" },
		"missing request path":       func(record *toolCallRecord) { record.Path = "" },
	} {
		t.Run(name, func(t *testing.T) {
			records := make([]toolCallRecord, earlierCalls+1)
			for index := range records {
				at := start.Add(time.Duration(index) * callGap)
				records[index] = replayRecord(at, at.Add(callDuration), uint64(index+1))
			}
			change(&records[earlierCalls])
			source := writeReplaySource(t, records, start.Add(time.Duration(len(records))*callGap), core.Evaluation{Status: core.StatusSucceeded})
			plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
			if err == nil || len(plan.Calls) != 0 {
				t.Fatalf("unreplayable call accepted: plan = %+v, error = %v", plan, err)
			}
		})
	}
}

func TestLoadReplaySourceRejectsIncompleteEvidenceAndWrongTask(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const callGap = time.Second
	const finalWait = 2 * time.Second
	records := []toolCallRecord{
		replayRecord(start, start.Add(callGap), 1),
		replayRecord(start.Add(callGap), start.Add(2*callGap), 2),
	}
	for name, change := range map[string]func(string){
		"complete-line truncation": func(source string) {
			path := filepath.Join(source, "bridge", "tool-calls.jsonl")
			log, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			firstLine := append([]byte(nil), log[:bytes.IndexByte(log, '\n')+1]...)
			if err := os.WriteFile(path, firstLine, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unfinalized source": func(source string) { updateReplayMetadata(t, source, "replay_ready", false) },
		"old schema":         func(source string) { updateReplayMetadata(t, source, "schema_version", 0) },
		"missing completion": func(source string) { updateReplayMetadata(t, source, "harness_completed_at", "") },
		"wrong benchmark":    func(source string) { updateReplayMetadata(t, source, "benchmark_type", "other") },
		"wrong logical task": func(source string) { updateReplayMetadata(t, source, "task_id", "other") },
		"wrong image":        func(source string) { updateReplayMetadata(t, source, "image", "other:tag") },
		"wrong revision": func(source string) {
			updateReplayMetadata(t, source, "benchmark_revisions", map[string]string{"revision": "other"})
		},
		"missing source evaluation": func(source string) {
			writeReplayJSON(t, filepath.Join(filepath.Dir(source), "run-result.json"), core.RunResult{RunID: replayRunID, Tasks: []core.TaskResult{{TaskID: replayTaskID}}})
		},
		"unknown evaluation status": func(source string) {
			writeReplayJSON(t, filepath.Join(filepath.Dir(source), "run-result.json"), core.RunResult{RunID: replayRunID, Tasks: []core.TaskResult{{TaskID: replayTaskID, Evaluation: core.Evaluation{Status: "unknown"}}}})
		},
		"duplicate source evaluation": func(source string) {
			writeReplayJSON(t, filepath.Join(filepath.Dir(source), "run-result.json"), core.RunResult{RunID: replayRunID, Tasks: []core.TaskResult{{TaskID: replayTaskID, Evaluation: core.Evaluation{Status: core.StatusSucceeded}}, {TaskID: replayTaskID, Evaluation: core.Evaluation{Status: core.StatusSucceeded}}}})
		},
		"negative final wait": func(source string) {
			updateReplayMetadata(t, source, "harness_completed_at", bridgekit.FormatTime(start))
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := writeReplaySource(t, records, start.Add(2*callGap+finalWait), core.Evaluation{Status: core.StatusSucceeded})
			change(source)
			plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
			if err == nil || len(plan.Calls) != 0 {
				t.Fatalf("unreplayable source accepted: plan = %+v, error = %v", plan, err)
			}
		})
	}
}

func TestPreflightReplaySourceSelectsOccurrences(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const otherTaskID = "other-task"
	const otherImage = "other-image:tag"
	const otherWorkdir = "/other-workdir"
	const repeatedOccurrenceID = "task-002"
	const otherOccurrenceID = "other-task-001"
	const emptyOccurrenceID = "empty-task-001"
	const startGap = 3 * time.Second
	otherTask := core.Task{ID: otherTaskID, Environment: core.Environment{Image: otherImage, Workdir: otherWorkdir}}
	other := replayRecord(start, start, 1)
	other.OperationClass, other.Command = kindExec, "pwd"
	other.CommandHash, other.Path, other.Workdir = bridgekit.CommandHash(other.Command), bridgekit.RemoteShellPath, otherWorkdir
	other.Argv = []string{bridgekit.RemoteShellPath, "-c", other.Command}
	inputs := []replayOccurrenceFixture{
		{id: replayTaskID, task: replayTask, records: []toolCallRecord{
			replayRecord(start.Add(startGap), start.Add(startGap), 1),
			replayRecord(start.Add(startGap), start.Add(startGap), 2),
		}},
		{id: repeatedOccurrenceID, task: replayTask, records: []toolCallRecord{replayRecord(start, start, 1)}},
		{id: otherOccurrenceID, task: otherTask, records: []toolCallRecord{other}},
		{id: emptyOccurrenceID, task: replayTask},
	}
	run := writeReplayRun(t, inputs, start.Add(startGap))
	catalog := []core.Task{replayTask, otherTask}
	selection, err := PreflightReplaySource(context.Background(), run, catalog, replayBenchmark, replayRevisions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Occurrences) != len(inputs) || !selection.StartedAt.Equal(start) {
		t.Fatalf("selected workload = %+v", selection)
	}
	for index, input := range inputs {
		occurrence := selection.Occurrences[index]
		if occurrence.SourceTaskID != input.id || occurrence.LogicalTaskID != input.task.ID || len(occurrence.Calls) != len(input.records) {
			t.Fatalf("selected occurrence = %+v", occurrence)
		}
		plan, err := selection.Load(context.Background(), index)
		if err != nil || plan.SourceTaskID != input.id || len(plan.Calls) != len(input.records) || plan.Evaluation.Status != core.StatusFailed {
			t.Fatalf("admitted occurrence = %+v, %v", plan, err)
		}
		if len(plan.Calls) != 0 {
			wantOffset := time.Duration(0)
			if index == 0 {
				wantOffset = startGap
			}
			if !plan.StartedAt.Equal(start) {
				t.Fatalf("global offsets have an occurrence-local source origin: %+v", plan)
			}
			for callIndex, call := range plan.Calls {
				if call.StartOffset != wantOffset || occurrence.Calls[callIndex].StartOffset != wantOffset {
					t.Fatalf("global call offsets = %+v, %+v", plan.Calls, occurrence.Calls)
				}
				if got := plan.StartedAt.Add(call.StartOffset); !got.Equal(start.Add(wantOffset)) {
					t.Fatalf("absolute call start was shifted twice: got %s", got)
				}
			}
		} else if !plan.StartedAt.IsZero() || !occurrence.StartedAt.IsZero() || plan.FinalWait != 0 {
			t.Fatalf("empty occurrence invented timing: %+v, %+v", plan, occurrence)
		}
	}
	selection.Occurrences[0] = ReplayOccurrence{}
	selection.StartedAt = start.Add(startGap)
	boundPlan, err := selection.Load(context.Background(), 0)
	if err != nil || boundPlan.SourceTaskID != replayTaskID || !boundPlan.StartedAt.Equal(start) || boundPlan.Calls[0].StartOffset != startGap {
		t.Fatalf("summary mutation changed the private admission binding: %+v, %v", boundPlan, err)
	}
	source := filepath.Join(run, replayTaskID)
	single, err := PreflightReplaySource(context.Background(), source, catalog, replayBenchmark, replayRevisions, nil)
	if err != nil || len(single.Occurrences) != 1 || !single.StartedAt.Equal(start.Add(startGap)) {
		t.Fatalf("single source selection = %+v, %v", single, err)
	}
	plan, err := single.Load(context.Background(), 0)
	if err != nil || plan.Calls[0].StartOffset != 0 {
		t.Fatalf("single source admission = %+v, %v", plan, err)
	}
}

func TestPreflightReplaySourceWarnsCrossOccurrenceStarts(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const secondOccurrenceID = "task-002"
	const privatePath = "/private/recorded-input"
	const owningRunID = "replay-run"
	const firstSequence uint64 = 1
	const secondSequence = firstSequence + 1
	const tieOffset = time.Second
	first := replayRecord(start, start, firstSequence)
	tied := replayRecord(start.Add(tieOffset), start.Add(tieOffset), secondSequence)
	tied.Path = privatePath
	other := replayRecord(start.Add(tieOffset), start.Add(tieOffset), firstSequence)
	other.Path = privatePath
	run := writeReplayRun(t, []replayOccurrenceFixture{
		{id: replayTaskID, task: replayTask, records: []toolCallRecord{first, tied}},
		{id: secondOccurrenceID, task: replayTask, records: []toolCallRecord{other}},
	}, start.Add(tieOffset))
	var output bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&output)
	logger.SetFormatter(&logrus.JSONFormatter{})
	selection, err := PreflightReplaySource(context.Background(), run, []core.Task{replayTask}, replayBenchmark, replayRevisions, logger.WithField("run_id", owningRunID))
	if err != nil || selection == nil {
		t.Fatalf("tied selection = %+v, %v", selection, err)
	}
	var warning struct {
		Level          string        `json:"level"`
		Message        string        `json:"msg"`
		RunID          string        `json:"run_id"`
		SourceRunID    string        `json:"source_run_id"`
		SourceTaskID   string        `json:"source_task_id"`
		SourceSequence uint64        `json:"source_sequence"`
		TiedTaskID     string        `json:"tied_source_task_id"`
		TiedSequence   uint64        `json:"tied_source_sequence"`
		StartedAt      string        `json:"started_at"`
		StartOffset    time.Duration `json:"start_offset_ns"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &warning); err != nil {
		t.Fatalf("cross-occurrence warning is missing or invalid: %q, %v", output.String(), err)
	}
	if warning.Level != logrus.WarnLevel.String() || warning.Message != "replay calls have equal start timestamps" || warning.RunID != owningRunID || warning.SourceRunID != replayRunID || warning.SourceTaskID != replayTaskID || warning.SourceSequence != secondSequence || warning.TiedTaskID != secondOccurrenceID || warning.TiedSequence != firstSequence || warning.StartedAt != bridgekit.FormatTime(start.Add(tieOffset)) || warning.StartOffset != tieOffset {
		t.Fatalf("cross-occurrence warning lost identity or timing: %s", output.String())
	}
	if strings.Contains(output.String(), privatePath) || strings.Contains(output.String(), replayOutcome) {
		t.Fatalf("cross-occurrence warning contains private payloads: %s", output.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger.SetOutput(cancelReplayLogWriter{cancel: cancel})
	if got, err := PreflightReplaySource(ctx, run, []core.Task{replayTask}, replayBenchmark, replayRevisions, logger.WithField("run_id", owningRunID)); err != context.Canceled || got != nil {
		t.Fatalf("cancellation during preflight exposed a selection: %+v, %v", got, err)
	}
}

type cancelReplayLogWriter struct {
	cancel context.CancelFunc
}

func (writer cancelReplayLogWriter) Write(data []byte) (int, error) {
	writer.cancel()
	return len(data), nil
}

func TestPreflightReplaySourceRejectsInvalidFinalOccurrence(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const finalID = "task-002"
	inputs := []replayOccurrenceFixture{
		{id: replayTaskID, task: replayTask, records: []toolCallRecord{replayRecord(start, start, 1)}},
		{id: finalID, task: replayTask, records: []toolCallRecord{replayRecord(start, start, 1)}},
	}
	for name, change := range map[string]func(string){
		"missing occurrence": func(run string) {
			if err := os.Rename(filepath.Join(run, finalID), filepath.Join(run, "unlisted")); err != nil {
				t.Fatal(err)
			}
		},
		"duplicate occurrence": func(run string) { rewriteReplayIndex(t, run, finalID, true) },
		"empty occurrence":     func(run string) { rewriteReplayIndex(t, run, "", false) },
		"escaped occurrence":   func(run string) { rewriteReplayIndex(t, run, "../"+finalID, false) },
		"absolute occurrence":  func(run string) { rewriteReplayIndex(t, run, filepath.Join(run, finalID), false) },
		"nested occurrence":    func(run string) { rewriteReplayIndex(t, run, finalID+"/child", false) },
		"dot occurrence":       func(run string) { rewriteReplayIndex(t, run, ".", false) },
		"symlink occurrence": func(run string) {
			path := filepath.Join(run, finalID)
			other := filepath.Join(run, "unlisted")
			if err := os.Rename(path, other); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, path); err != nil {
				t.Fatal(err)
			}
		},
		"mismatched occurrence": func(run string) {
			rewriteReplayRecord(t, filepath.Join(run, finalID), func(record map[string]any) { record["task_id"] = replayTaskID })
		},
		"mismatched logical task": func(run string) { updateReplayMetadata(t, filepath.Join(run, finalID), "task_id", "unknown") },
		"mismatched provenance":   func(run string) { updateReplayMetadata(t, filepath.Join(run, finalID), "image", "other:image") },
		"mismatched run":          func(run string) { updateReplayMetadata(t, filepath.Join(run, finalID), "run_id", "other-run") },
		"incomplete evidence":     func(run string) { updateReplayMetadata(t, filepath.Join(run, finalID), "replay_ready", false) },
		"missing evaluation": func(run string) {
			writeReplayJSON(t, filepath.Join(run, "run-result.json"), core.RunResult{RunID: replayRunID, Tasks: []core.TaskResult{
				{TaskID: replayTaskID, Evaluation: core.Evaluation{Status: core.StatusSucceeded}}, {TaskID: finalID},
			}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			run := writeReplayRun(t, inputs, start)
			change(run)
			selection, err := PreflightReplaySource(context.Background(), run, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil)
			if err == nil || selection != nil {
				t.Fatalf("invalid final occurrence exposed an admissible selection: %+v, %v", selection, err)
			}
		})
	}
}

func rewriteReplayIndex(t *testing.T, run, finalID string, duplicate bool) {
	t.Helper()
	tasks := []core.TaskResult{
		{TaskID: replayTaskID, Evaluation: core.Evaluation{Status: core.StatusSucceeded}},
		{TaskID: finalID, Evaluation: core.Evaluation{Status: core.StatusSucceeded}},
	}
	if duplicate {
		tasks = append(tasks, tasks[len(tasks)-1])
	}
	writeReplayJSON(t, filepath.Join(run, "run-result.json"), core.RunResult{RunID: replayRunID, Tasks: tasks})
}

func TestReplaySelectionRejectsChangedFinalizedEvidence(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(string){
		"refinalized call": func(source string) {
			rewriteReplayRecord(t, source, func(record map[string]any) { record["path"] = "/changed-input" })
		},
		"runtime completion": func(source string) {
			updateReplayMetadata(t, source, "harness_completed_at", bridgekit.FormatTime(start.Add(time.Second)))
		},
		"parent result": func(source string) {
			writeReplayJSON(t, filepath.Join(filepath.Dir(source), "run-result.json"), core.RunResult{RunID: replayRunID, Name: "changed", Tasks: []core.TaskResult{
				{TaskID: replayTaskID, Evaluation: core.Evaluation{Status: core.StatusSucceeded}},
			}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := writeReplaySource(t, []toolCallRecord{replayRecord(start, start, 1)}, start, core.Evaluation{Status: core.StatusSucceeded})
			selection, err := PreflightReplaySource(context.Background(), source, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil)
			if err != nil {
				t.Fatal(err)
			}
			change(source)
			plan, err := selection.Load(context.Background(), 0)
			if err == nil || !strings.Contains(err.Error(), "changed after preflight") || plan.SourceRunID != "" || len(plan.Calls) != 0 {
				t.Fatalf("changed finalized source admitted: %+v, %v", plan, err)
			}
		})
	}
}

func TestPreflightReplaySourceEmptyAndLegacySources(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const secondOccurrenceID = "task-002"
	run := writeReplayRun(t, []replayOccurrenceFixture{{id: replayTaskID, task: replayTask}, {id: secondOccurrenceID, task: replayTask}}, start)
	selection, err := PreflightReplaySource(context.Background(), run, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.StartedAt.IsZero() {
		t.Fatalf("empty workload invented a source origin: %+v", selection)
	}
	for index, occurrence := range selection.Occurrences {
		plan, err := selection.Load(context.Background(), index)
		if err != nil || len(occurrence.Calls) != 0 || !occurrence.StartedAt.IsZero() || len(plan.Calls) != 0 || plan.FinalWait != 0 || !plan.StartedAt.IsZero() {
			t.Fatalf("empty source admission = %+v, %+v, %v", occurrence, plan, err)
		}
	}
	source := writeReplaySource(t, []toolCallRecord{replayRecord(start, start, 1)}, start, core.Evaluation{Status: core.StatusSucceeded})
	updateReplayMetadata(t, source, "run_id", nil)
	before := make(map[string][]byte)
	for _, path := range []string{filepath.Join(source, "bridge", "runtime.json"), filepath.Join(source, "bridge", "tool-calls.jsonl"), filepath.Join(filepath.Dir(source), "run-result.json")} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	legacy, err := PreflightReplaySource(context.Background(), source, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := legacy.Load(context.Background(), 0)
	if err != nil || len(plan.Calls) != 1 {
		t.Fatalf("legacy source admission = %+v, %v", plan, err)
	}
	for path, original := range before {
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, original) {
			t.Fatalf("source evidence changed: %s, %v", path, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := PreflightReplaySource(ctx, source, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil); err != context.Canceled || got != nil {
		t.Fatalf("canceled preflight = %+v, %v", got, err)
	}
	if got, err := legacy.Load(ctx, 0); err != context.Canceled || len(got.Calls) != 0 {
		t.Fatalf("canceled admission = %+v, %v", got, err)
	}
}

func TestPreflightReplaySourceUsesRecordedGlobalTimeBounds(t *testing.T) {
	const earlyTimestamp = "0001-01-01T00:00:00Z"
	const lateTimestamp = "9999-01-01T00:00:00Z"
	const laterOccurrenceID = "task-002"
	early, err := time.Parse(time.RFC3339Nano, earlyTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	late, err := time.Parse(time.RFC3339Nano, lateTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	for name, later := range map[string]time.Time{"zero source time": early.Add(time.Second), "unrepresentable offset": late} {
		t.Run(name, func(t *testing.T) {
			// Completion markers are local to each source, not the run timeline.
			firstSource := writeReplaySource(t, []toolCallRecord{replayRecord(early, early, 1)}, early, core.Evaluation{Status: core.StatusSucceeded})
			second := replayRecord(later, later, 1)
			second.TaskID = laterOccurrenceID
			secondSource := writeReplaySource(t, []toolCallRecord{second}, later, core.Evaluation{Status: core.StatusSucceeded})
			run := filepath.Dir(firstSource)
			if err := os.Rename(secondSource, filepath.Join(run, laterOccurrenceID)); err != nil {
				t.Fatal(err)
			}
			rewriteReplayIndex(t, run, laterOccurrenceID, false)
			selection, err := PreflightReplaySource(context.Background(), run, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil)
			if later.Equal(late) {
				if err == nil || selection != nil {
					t.Fatalf("unrepresentable global offset accepted: %+v, %v", selection, err)
				}
				return
			}
			if err != nil || !selection.StartedAt.Equal(early) {
				t.Fatalf("recorded zero origin treated as an empty trace: %+v, %v", selection, err)
			}
			plan, err := selection.Load(context.Background(), len(selection.Occurrences)-1)
			if err != nil || plan.Calls[0].StartOffset != time.Second || !plan.StartedAt.Equal(early) {
				t.Fatalf("recorded global source origin and offset = %+v, %v", plan, err)
			}
		})
	}
}

func TestPreflightReplaySourceChecksGlobalPlanBound(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const laterOccurrenceID = "task-002"
	const callGap = time.Second
	run := writeReplayRun(t, []replayOccurrenceFixture{
		{id: replayTaskID, task: replayTask, records: []toolCallRecord{replayRecord(start, start, 1)}},
		{id: laterOccurrenceID, task: replayTask, records: []toolCallRecord{replayRecord(start.Add(callGap), start.Add(callGap), 1)}},
	}, start.Add(callGap))
	updateReplayMetadata(t, filepath.Join(run, replayTaskID), "harness_completed_at", bridgekit.FormatTime(start))
	plan, err := LoadReplaySource(filepath.Join(run, laterOccurrenceID), replayTask, replayBenchmark, replayRevisions)
	if err != nil {
		t.Fatal(err)
	}
	localData, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Calls[0].StartOffset = callGap
	const replayOrigin = "2026-11-05T12:00:00.000000001Z"
	replayAt, err := time.Parse(time.RFC3339Nano, replayOrigin)
	if err != nil {
		t.Fatal(err)
	}
	plan.ReplayAt = &replayAt
	globalData, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	const oneByte = 1
	for _, bound := range []int{len(localData), len(globalData) - oneByte, len(globalData)} {
		selection, err := preflightReplaySource(context.Background(), run, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil, bound)
		if bound < len(globalData) {
			if err == nil || selection != nil {
				t.Fatalf("global plan above encoded bound accepted: %+v, %v", selection, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := selection.Load(context.Background(), len(selection.Occurrences)-1)
		if err != nil || got.Calls[0].StartOffset != callGap {
			t.Fatalf("exact global plan bound rejected: %+v, %v", got, err)
		}
		got.ReplayAt = &replayAt
		encoded, err := json.Marshal(got)
		if err != nil || len(encoded) != bound {
			t.Fatalf("preflight did not cover serialized origin: bytes=%d bound=%d err=%v", len(encoded), bound, err)
		}
	}
}

func TestPreflightReplaySourceKeepsLocalPlanBounds(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		records []toolCallRecord
	}{
		{name: "empty"},
		{name: "calls", records: []toolCallRecord{replayRecord(start, start, 1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := writeReplaySource(t, test.records, start, core.Evaluation{Status: core.StatusSucceeded})
			plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			const oneByte = 1
			for _, bound := range []int{len(data) - oneByte, len(data)} {
				selection, err := preflightReplaySource(context.Background(), source, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil, bound)
				if bound < len(data) {
					if err == nil || selection != nil {
						t.Fatalf("local plan exceeded preflight bound: %+v %v", selection, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := selection.Load(context.Background(), 0)
				if err != nil || len(got.Calls) != len(test.records) || got.ReplayAt != nil {
					t.Fatalf("local plan bound changed: %+v %v", got, err)
				}
			}
		})
	}
}

func TestReplaySelectionDoesNotRetainPayloadPlans(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const payloadBytes = 2 << 20
	const occurrenceCount = 8
	const retainedPayloadLimit = 2 * payloadBytes
	inputs := make([]replayOccurrenceFixture, occurrenceCount)
	for index := range inputs {
		record := replayRecord(start, start, 1)
		record.OperationClass, record.Stdin = kindFileWrite, strings.Repeat("x", payloadBytes)
		record.StdinBytes = int64(len(record.Stdin))
		digest := sha256.Sum256([]byte(record.Stdin))
		record.SHA256 = hex.EncodeToString(digest[:])
		inputs[index] = replayOccurrenceFixture{id: replayTaskID + strings.Repeat("-repeat", index), task: replayTask, records: []toolCallRecord{record}}
	}
	run := writeReplayRun(t, inputs, start)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	selection, err := PreflightReplaySource(context.Background(), run, []core.Task{replayTask}, replayBenchmark, replayRevisions, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained >= retainedPayloadLimit {
		t.Fatalf("selection retained payload-sized heap: %d bytes", retained)
	}
	for index := range selection.Occurrences {
		plan, err := selection.Load(context.Background(), index)
		if err != nil || len(plan.Calls) != 1 || len(plan.Calls[0].Body) != payloadBytes {
			t.Fatalf("bounded payload admission = %d calls, %v", len(plan.Calls), err)
		}
	}
	runtime.KeepAlive(selection)
}

type replayOccurrenceFixture struct {
	id      string
	task    core.Task
	records []toolCallRecord
}

func writeReplayRun(t *testing.T, inputs []replayOccurrenceFixture, completed time.Time) string {
	t.Helper()
	run := t.TempDir()
	if err := os.Chmod(run, replayDirectoryMode); err != nil {
		t.Fatal(err)
	}
	result := core.RunResult{RunID: replayRunID}
	for _, input := range inputs {
		records := slices.Clone(input.records)
		for index := range records {
			records[index].TaskID = input.id
		}
		evaluation := core.Evaluation{Status: core.StatusFailed}
		source := writeReplaySource(t, records, completed, evaluation)
		updateReplayMetadata(t, source, "task_id", input.task.ID)
		updateReplayMetadata(t, source, "image", input.task.Environment.Image)
		if err := os.Rename(source, filepath.Join(run, input.id)); err != nil {
			t.Fatal(err)
		}
		result.Tasks = append(result.Tasks, core.TaskResult{TaskID: input.id, Evaluation: evaluation})
	}
	writeReplayJSON(t, filepath.Join(run, "run-result.json"), result)
	return run
}

func updateReplayMetadata(t *testing.T, source, key string, value any) {
	t.Helper()
	path := filepath.Join(source, "bridge", "runtime.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal(content, &metadata); err != nil {
		t.Fatal(err)
	}
	if value == nil {
		delete(metadata, key)
	} else {
		metadata[key] = value
	}
	writeReplayJSON(t, path, metadata)
}

func TestLoadReplaySourceChecksEncodedPlanBound(t *testing.T) {
	at, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const payloadBytes = 4 << 10
	const boundaryByte = 1
	const firstSequence = 1
	const callGap = time.Second
	write := replayRecord(at, at, firstSequence)
	write.OperationClass, write.Stdin = kindFileWrite, strings.Repeat("a", payloadBytes)
	write.StdinBytes = int64(len(write.Stdin))
	digest := sha256.Sum256([]byte(write.Stdin))
	write.SHA256 = hex.EncodeToString(digest[:])
	stat := replayRecord(at.Add(callGap), at.Add(callGap), firstSequence+1)
	stat.Path = "/" + strings.Repeat("<", payloadBytes)
	for name, records := range map[string][]toolCallRecord{
		"escaping and base64": {write, stat},
		"empty":               nil,
	} {
		t.Run(name, func(t *testing.T) {
			source := writeReplaySource(t, records, at.Add(callGap+callGap), core.Evaluation{Status: core.StatusSucceeded})
			plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			log, err := os.ReadFile(filepath.Join(source, "bridge", "tool-calls.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if len(log) >= len(data)-boundaryByte || int64(len(log)) > maxReplayLogBytes {
				t.Fatalf("audit must fit below the plan bound: audit=%d, plan=%d", len(log), len(data))
			}
			for _, bound := range []int{len(data) - boundaryByte, len(data)} {
				got, err := loadReplaySource(source, replayTask, replayBenchmark, replayRevisions, bound)
				if bound == len(data) {
					if err != nil || got.SourceRunID != replayRunID || len(got.Calls) != len(records) || got.FinalWait != plan.FinalWait {
						t.Fatalf("exact plan bound rejected: %+v, %v", got, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "plan") || got.SourceRunID != "" || len(got.Calls) != 0 {
					t.Fatalf("oversized plan accepted: bound=%d, plan=%d, error=%v", bound, len(data), err)
				}
			}
		})
	}
}

func TestLoadReplaySourceBindsParentRun(t *testing.T) {
	at, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const otherRunID = "other-run"
	for _, test := range []struct {
		name     string
		records  []toolCallRecord
		runID    any
		parentID string
		valid    bool
	}{
		{name: "bound empty", runID: replayRunID, parentID: replayRunID, valid: true},
		{name: "copied empty", runID: replayRunID, parentID: otherRunID},
		{name: "legacy empty", parentID: replayRunID},
		{name: "legacy nonempty", records: []toolCallRecord{replayRecord(at, at, 1)}, parentID: replayRunID, valid: true},
		{name: "copied legacy nonempty", records: []toolCallRecord{replayRecord(at, at, 1)}, parentID: otherRunID},
		{name: "wrong metadata on nonempty", records: []toolCallRecord{replayRecord(at, at, 1)}, runID: otherRunID, parentID: replayRunID},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := writeReplaySource(t, test.records, at, core.Evaluation{Status: core.StatusSucceeded})
			updateReplayMetadata(t, source, "run_id", test.runID)
			writeReplayJSON(t, filepath.Join(filepath.Dir(source), "run-result.json"), core.RunResult{
				RunID: test.parentID, Tasks: []core.TaskResult{{TaskID: replayTaskID, Evaluation: core.Evaluation{Status: core.StatusSucceeded}}},
			})
			plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
			if test.valid {
				if err != nil || plan.SourceRunID != replayRunID || plan.Evaluation.Status != core.StatusSucceeded {
					t.Fatalf("bound source = %+v, error = %v", plan, err)
				}
			} else if err == nil || plan.SourceRunID != "" || len(plan.Calls) != 0 {
				t.Fatalf("unbound source accepted: %+v, error = %v", plan, err)
			} else if len(test.records) == 0 && !strings.Contains(err.Error(), "source run binding") {
				t.Fatalf("unclear empty source error: %v", err)
			}
		})
	}
}

func TestLoadReplaySourceEmptyTraceHasNoFinalWait(t *testing.T) {
	at, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	source := writeReplaySource(t, nil, at, core.Evaluation{Status: core.StatusSucceeded})
	plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
	if err != nil || len(plan.Calls) != 0 || plan.FinalWait != 0 || plan.SourceRunID != replayRunID || plan.Evaluation.Status != core.StatusSucceeded {
		t.Fatalf("empty replay plan = %+v, error = %v", plan, err)
	}
}

func TestLoadReplaySourceReplaysEqualStartsWithOverlap(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const callDuration = time.Second
	const firstSequence uint64 = 1
	const secondSequence = firstSequence + 1
	response := &sandboxv1.StatResponse{}
	expected := expectedReplayCall("Stat", response)
	records := []toolCallRecord{
		replayRecord(start, start.Add(callDuration), firstSequence),
		replayRecord(start, start.Add(callDuration+callDuration), secondSequence),
	}
	for index := range records {
		records[index].ResponseBytes = expected.ResponseBytes
		records[index].ResponseSHA256 = expected.ResponseSHA256
	}
	source := writeReplaySource(t, records, start.Add(callDuration+callDuration), core.Evaluation{Status: core.StatusSucceeded})
	plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
	if err != nil {
		t.Fatal(err)
	}
	sequences := make(map[uint64]bool, len(records))
	for _, call := range plan.Calls {
		if call.StartOffset != 0 || sequences[call.Sequence] {
			t.Fatalf("tied call lost its offset or identity: %+v", plan.Calls)
		}
		sequences[call.Sequence] = true
	}
	if len(plan.Calls) != len(records) || !sequences[firstSequence] || !sequences[secondSequence] || plan.FinalWait != 0 || !plan.StartedAt.Equal(start) {
		t.Fatalf("tied call plan = %+v", plan)
	}

	entered := make(chan struct{}, len(records))
	release := make(chan struct{}, len(records))
	defer close(release)
	server := &replayServer{stat: func(ctx context.Context, _ *sandboxv1.StatRequest) (*sandboxv1.StatResponse, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return response, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	client := replayEndpoint(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), replayTestDeadline)
	defer cancel()
	const callerResults = 1
	done := make(chan error, callerResults)
	go func() { done <- ReplayCalls(ctx, replayTestDeadline, client, plan) }()
	for range records {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("tied calls did not overlap")
		}
	}
	// Both calls are active before either can finish.
	for range records {
		release <- struct{}{}
	}
	if err := <-done; err != nil {
		t.Fatalf("replay tied calls: %v", err)
	}
}

func TestLoadReplaySourceRejectsBadEqualStartEvidence(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const firstSequence uint64 = 1
	const secondSequence = firstSequence + 1
	const callDuration = time.Second
	for name, change := range map[string]func(*toolCallRecord){
		"sequence gap":       func(record *toolCallRecord) { record.Sequence = secondSequence + 1 },
		"duplicate sequence": func(record *toolCallRecord) { record.Sequence = firstSequence },
		"invalid start":      func(record *toolCallRecord) { record.StartedAt = "invalid" },
		"invalid completion": func(record *toolCallRecord) { record.Timestamp = "invalid" },
		"completion before start": func(record *toolCallRecord) {
			record.Timestamp = bridgekit.FormatTime(start.Add(-callDuration))
		},
		"incomplete response": func(record *toolCallRecord) { record.ResponseSHA256 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			records := []toolCallRecord{
				replayRecord(start, start.Add(callDuration), firstSequence),
				replayRecord(start, start.Add(callDuration), secondSequence),
			}
			change(&records[len(records)-1])
			source := writeReplaySource(t, records, start.Add(callDuration), core.Evaluation{Status: core.StatusSucceeded})
			if plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions); err == nil || len(plan.Calls) != 0 {
				t.Fatalf("bad equal-start evidence accepted: plan=%+v error=%v", plan, err)
			}
		})
	}
}

func TestLoadReplaySourceRejectsUnrepresentableTiming(t *testing.T) {
	const earlyTimestamp = "0001-01-01T00:00:00Z"
	const lateTimestamp = "9999-01-01T00:00:00Z"
	early, err := time.Parse(time.RFC3339Nano, earlyTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	late, err := time.Parse(time.RFC3339Nano, lateTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	for name, records := range map[string][]toolCallRecord{
		"start offset": {replayRecord(early, early, 1), replayRecord(late, late, 2)},
		"final wait":   {replayRecord(early, early, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			source := writeReplaySource(t, records, late, core.Evaluation{Status: core.StatusSucceeded})
			if plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions); err == nil || len(plan.Calls) != 0 {
				t.Fatalf("unrepresentable timing accepted: plan = %+v, error = %v", plan, err)
			}
		})
	}
}

func TestLoadReplaySourceKeepsFileRequestsAndRecordedFailure(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const gap = time.Second
	records := make([]toolCallRecord, 4)
	for index := range records {
		at := start.Add(time.Duration(index) * gap)
		records[index] = replayRecord(at, at.Add(gap), uint64(index+1))
	}
	records[0].Status, records[0].GRPCCode = "failed", "NotFound"
	records[1].OperationClass = kindFileRead
	records[1].MaxBytes = 1
	records[2].OperationClass = kindFileRead
	records[2].FirstLine, records[2].MaxLines = 2, 3
	records[3].OperationClass, records[3].Stdin = kindFileWrite, "body"
	records[3].StdinBytes = int64(len(records[3].Stdin))
	digest := sha256.Sum256([]byte(records[3].Stdin))
	records[3].SHA256 = hex.EncodeToString(digest[:])
	const finalWait = 2 * time.Second
	source := writeReplaySource(t, records, start.Add(time.Duration(len(records))*gap+finalWait), core.Evaluation{Status: core.StatusFailed})
	plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Calls) != len(records) || plan.Calls[0].Operation != "Stat" || plan.Calls[0].ResponseCode != records[0].GRPCCode || plan.Calls[1].Operation != "ReadFile" || plan.Calls[1].MaxBytes != records[1].MaxBytes || plan.Calls[2].Operation != "ReadLines" || plan.Calls[2].FirstLine != records[2].FirstLine || plan.Calls[2].MaxLines != records[2].MaxLines || plan.Calls[3].Operation != "WriteFile" || string(plan.Calls[3].Body) != records[3].Stdin || plan.FinalWait != finalWait || plan.Evaluation.Status != core.StatusFailed {
		t.Fatalf("file request plan = %+v", plan)
	}
}

func TestLoadReplaySourceRejectsMissingFieldsAndUnsafeSourceFiles(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const callLength = time.Second
	for name, change := range map[string]func(string){
		"missing stdin":      func(source string) { deleteReplayRecordField(t, source, "stdin") },
		"missing started_at": func(source string) { deleteReplayRecordField(t, source, "started_at") },
		"partial final line": func(source string) {
			path := filepath.Join(source, "bridge", "tool-calls.jsonl")
			log, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, log[:len(log)-1], replayFileMode); err != nil {
				t.Fatal(err)
			}
		},
		"symlink log": func(source string) {
			path := filepath.Join(source, "bridge", "tool-calls.jsonl")
			other := filepath.Join(source, "other-log")
			log, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(other, log, replayFileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, path); err != nil {
				t.Fatal(err)
			}
		},
		"public log": func(source string) {
			if err := os.Chmod(filepath.Join(source, "bridge", "tool-calls.jsonl"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"missing result": func(source string) {
			if err := os.Remove(filepath.Join(filepath.Dir(source), "run-result.json")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			source := writeReplaySource(t, []toolCallRecord{replayRecord(start, start.Add(callLength), 1)}, start.Add(callLength), core.Evaluation{Status: core.StatusSucceeded})
			change(source)
			plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
			if err == nil || len(plan.Calls) != 0 {
				t.Fatalf("unsafe replay source accepted: plan = %+v, error = %v", plan, err)
			}
		})
	}
}

func TestLoadReplaySourceAcceptsRecordedGRPCCalls(t *testing.T) {
	executor := newFakeExecutor(fstest.MapFS{"app/f": &fstest.MapFile{Data: []byte("x\n"), Mode: 0o600}})
	manager, client, endpoint := startBridge(t, executor, Options{CaptureReplay: true, OmitRawLog: true})
	for name, err := range callEveryRPC(client) {
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	stop(t, manager)
	log, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	var records []toolCallRecord
	for _, line := range bytes.Split(bytes.TrimSpace(log), []byte{'\n'}) {
		var record toolCallRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		record.RunID, record.TaskID, record.SandboxID = replayRunID, replayTaskID, replaySandboxID
		records = append(records, record)
	}
	completed, err := time.Parse(time.RFC3339Nano, records[len(records)-1].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	source := writeReplaySource(t, records, completed, core.Evaluation{Status: core.StatusSucceeded})
	plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Calls) != len(records) {
		t.Fatalf("recorded calls = %d, planned calls = %d", len(records), len(plan.Calls))
	}
	var operations []string
	for _, call := range plan.Calls {
		operations = append(operations, call.Operation)
	}
	if !slices.Equal(operations, []string{"Exec", "Stat", "ReadFile", "ReadLines", "WriteFile"}) {
		t.Fatalf("planned operations = %v", operations)
	}
}

func TestReplayCallsReplaysRecordedGRPCMethods(t *testing.T) {
	const callerDeadline = time.Minute
	const replayStep = 200 * time.Millisecond
	files := fstest.MapFS{"app/f": &fstest.MapFile{Data: []byte("x\n"), Mode: 0o600}}
	sourceTarget := newFakeExecutor(files)
	sourceManager := newTestManager(t, Options{CaptureReplay: true, OmitRawLog: true})
	sourceEndpoint, err := sourceManager.StartTarget(context.Background(), sourceTarget)
	if err != nil {
		t.Fatal(err)
	}
	sourceClient := dial(t, sourceEndpoint, withToolCallID("recorded-tool")...)
	for operation, callErr := range callEveryRPC(sourceClient) {
		if callErr != nil {
			t.Fatalf("record %s: %v", operation, callErr)
		}
	}
	stop(t, sourceManager)
	log, err := os.ReadFile(sourceEndpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	var records []toolCallRecord
	for _, line := range bytes.Split(bytes.TrimSpace(log), []byte{'\n'}) {
		var record toolCallRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		record.RunID, record.TaskID, record.SandboxID = replayRunID, replayTaskID, replaySandboxID
		records = append(records, record)
	}
	completed, err := time.Parse(time.RFC3339Nano, records[len(records)-1].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	source := writeReplaySource(t, records, completed, core.Evaluation{Status: core.StatusSucceeded})
	plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
	if err != nil {
		t.Fatal(err)
	}
	// Separate dependent file calls so a fast replay cannot overtake its write.
	for index := range plan.Calls {
		plan.Calls[index].StartOffset = time.Duration(index) * replayStep
	}
	target := newFakeExecutor(fstest.MapFS{"app/f": &fstest.MapFile{Data: []byte("x\n"), Mode: 0o600}})
	manager, client, endpoint := startBridge(t, target, Options{CaptureReplay: true, SandboxID: "new-sandbox", OmitRawLog: true})
	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()
	if err := ReplayCalls(ctx, callerDeadline, client, plan); err != nil {
		t.Fatalf("ReplayCalls: %v", err)
	}
	stop(t, manager)
	commands, stdins, fileOps := target.snapshot()
	if len(commands) != 1 || commands[0].Args[len(commands[0].Args)-1] != "true" || len(stdins) != 1 || fileOps != len(plan.Calls)-len(commands) {
		t.Fatalf("replayed commands = %#v, stdin = %q, file calls = %d", commands, stdins, fileOps)
	}
	newLog, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(newLog), []byte{'\n'}) {
		var record toolCallRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.SandboxID != "new-sandbox" || record.RunID != target.RunID() || record.TaskID != target.TaskID() || record.ToolCallID != "recorded-tool" {
			t.Fatalf("replayed call identity = %+v", record.ToolCallRecord)
		}
	}
}

func TestLoadReplaySourceAcceptsCapturedEmptyExec(t *testing.T) {
	manager, client, endpoint := startBridge(t, newFakeExecutor(nil), Options{CaptureReplay: true, OmitRawLog: true})
	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{}); err != nil {
		t.Fatal(err)
	}
	stop(t, manager)
	log, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(log), &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["command"]; present {
		t.Fatal("empty script unexpectedly appears in the recorded command field")
	}
	var record toolCallRecord
	if err := json.Unmarshal(bytes.TrimSpace(log), &record); err != nil {
		t.Fatal(err)
	}
	record.RunID, record.TaskID, record.SandboxID = replayRunID, replayTaskID, replaySandboxID
	completed, err := time.Parse(time.RFC3339Nano, record.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	source := writeReplaySource(t, []toolCallRecord{record}, completed, core.Evaluation{Status: core.StatusSucceeded})
	plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
	if err != nil || len(plan.Calls) != 1 || plan.Calls[0].Operation != "Exec" || plan.Calls[0].Script != "" {
		t.Fatalf("captured empty Exec = %+v, error = %v", plan, err)
	}
}

func TestLoadReplaySourceRejectsMissingOrNullRequestInputs(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const callLength = time.Second
	for name, change := range map[string]func(map[string]any){
		"missing Exec stdin bytes": func(record map[string]any) { delete(record, "stdin_bytes") },
		"null Exec stdin bytes":    func(record map[string]any) { record["stdin_bytes"] = nil },
		"null Exec stdin":          func(record map[string]any) { record["stdin"] = nil },
		"missing Exec command hash": func(record map[string]any) {
			delete(record, "command_hash")
		},
		"null write stdin": func(record map[string]any) {
			record["operation_class"] = kindFileWrite
			record["stdin"] = nil
			digest := sha256.Sum256(nil)
			record["sha256"] = hex.EncodeToString(digest[:])
		},
		"null write stdin bytes": func(record map[string]any) {
			record["operation_class"] = kindFileWrite
			record["stdin_bytes"] = nil
			digest := sha256.Sum256(nil)
			record["sha256"] = hex.EncodeToString(digest[:])
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := replayRecord(start, start.Add(callLength), 1)
			record.OperationClass, record.Path, record.Command = kindExec, bridgekit.RemoteShellPath, "true"
			record.CommandHash = bridgekit.CommandHash(record.Command)
			record.Workdir = testWorkdir
			record.Argv = []string{bridgekit.RemoteShellPath, "-c", record.Command}
			source := writeReplaySource(t, []toolCallRecord{record}, start.Add(callLength), core.Evaluation{Status: core.StatusSucceeded})
			rewriteReplayRecord(t, source, change)
			plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
			if err == nil || len(plan.Calls) != 0 {
				t.Fatalf("incomplete request accepted: plan = %+v, error = %v", plan, err)
			}
		})
	}
}

func TestLoadReplaySourceRejectsMissingOrNullEvaluationGrades(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(map[string]any){
		"missing score":  func(evaluation map[string]any) { delete(evaluation, "score") },
		"null score":     func(evaluation map[string]any) { evaluation["score"] = nil },
		"missing reward": func(evaluation map[string]any) { delete(evaluation, "reward") },
		"null reward":    func(evaluation map[string]any) { evaluation["reward"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			source := writeReplaySource(t, nil, start, core.Evaluation{Status: core.StatusSucceeded})
			evaluation := map[string]any{"status": core.StatusSucceeded, "score": 0, "reward": 0}
			change(evaluation)
			writeReplayJSON(t, filepath.Join(filepath.Dir(source), "run-result.json"), map[string]any{
				"run_id": replayRunID, "tasks": []any{map[string]any{"task_id": replayTaskID, "evaluation": evaluation}},
			})
			plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
			if err == nil || len(plan.Calls) != 0 {
				t.Fatalf("incomplete evaluation accepted: plan = %+v, error = %v", plan, err)
			}
		})
	}
}

func deleteReplayRecordField(t *testing.T, source, field string) {
	t.Helper()
	rewriteReplayRecord(t, source, func(record map[string]any) {
		if field == "stdin" {
			record["operation_class"] = kindFileWrite
			bodyDigest := sha256.Sum256(nil)
			record["sha256"] = hex.EncodeToString(bodyDigest[:])
		}
		delete(record, field)
	})
}

func rewriteReplayRecord(t *testing.T, source string, change func(map[string]any)) {
	t.Helper()
	path := filepath.Join(source, "bridge", "tool-calls.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &record); err != nil {
		t.Fatal(err)
	}
	change(record)
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	log := append(line, '\n')
	if err := os.WriteFile(path, log, replayFileMode); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(log)
	updateReplayMetadata(t, source, "tool_calls_bytes", len(log))
	updateReplayMetadata(t, source, "tool_calls_sha256", hex.EncodeToString(digest[:]))
}
