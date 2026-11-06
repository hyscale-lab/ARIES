package grpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/core"
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
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		log = append(append(log, line...), '\n')
	}
	if err := os.WriteFile(filepath.Join(bridgeDir, "tool-calls.jsonl"), log, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(log)
	metadata := map[string]any{
		"schema_version": 1, "replay_ready": true, "SandboxID": replaySandboxID,
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
	metadata[key] = value
	writeReplayJSON(t, path, metadata)
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

func TestLoadReplaySourceRejectsEqualStarts(t *testing.T) {
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	const callDuration = time.Second
	source := writeReplaySource(t, []toolCallRecord{
		replayRecord(start, start.Add(callDuration), 1),
		replayRecord(start, start.Add(callDuration), 2),
	}, start.Add(callDuration), core.Evaluation{Status: core.StatusSucceeded})
	if plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions); err == nil || len(plan.Calls) != 0 {
		t.Fatalf("equal call starts accepted: plan = %+v, error = %v", plan, err)
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
	manager, client, endpoint := startBridge(t, executor, Options{OmitRawLog: true})
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
	sourceManager := newTestManager(t, Options{OmitRawLog: true})
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
	manager, client, endpoint := startBridge(t, target, Options{SandboxID: "new-sandbox", OmitRawLog: true})
	ctx, cancel := context.WithTimeout(context.Background(), callerDeadline)
	defer cancel()
	if err := ReplayCalls(ctx, client, plan); err != nil {
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
	manager, client, endpoint := startBridge(t, newFakeExecutor(nil), Options{OmitRawLog: true})
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
