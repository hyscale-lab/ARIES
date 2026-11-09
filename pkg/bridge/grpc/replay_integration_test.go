//go:build integration

package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/sirupsen/logrus"
	"testing/fstest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	replayIntegrationWait         = 200 * time.Millisecond
	replayIntegrationScore        = 0.75
	replayIntegrationReward       = 1
	replayIntegrationCount        = 1
	replayIntegrationSourceOutput = "source"
)

type replayIntegrationBenchmark struct{ task core.Task }

func (benchmark replayIntegrationBenchmark) Tasks(context.Context) ([]core.Task, error) {
	return []core.Task{benchmark.task}, nil
}
func (replayIntegrationBenchmark) PrepareSandbox(context.Context, core.Task, runner.Sandbox) error {
	return nil
}
func (replayIntegrationBenchmark) Evaluate(context.Context, core.Task, runner.Sandbox, runner.EvaluationSandboxes) (core.Evaluation, error) {
	panic("replay must not run a verifier")
}

type replayIntegrationSandbox struct {
	target *fakeExecutor
	starts int
	stops  int
}

func (sandbox *replayIntegrationSandbox) Start(context.Context, core.SandboxRequest) (runner.Sandbox, error) {
	sandbox.starts++
	return sandbox, nil
}
func (sandbox *replayIntegrationSandbox) Stop(context.Context, runner.Sandbox) error {
	sandbox.stops++
	return nil
}
func (sandbox *replayIntegrationSandbox) Connectivity() core.HarnessConnectivity {
	return core.HarnessConnectivity{}
}
func (sandbox *replayIntegrationSandbox) Exec(context.Context, core.Command) (core.CommandResult, error) {
	panic("replay sandbox does not need direct exec")
}
func (sandbox *replayIntegrationSandbox) Upload(context.Context, string, string) error {
	panic("replay sandbox does not need direct upload")
}
func (sandbox *replayIntegrationSandbox) Download(context.Context, string, string) error {
	panic("replay sandbox does not need direct download")
}

type replayIntegrationBridge struct {
	manager *Manager
	sandbox *replayIntegrationSandbox
	stops   int
}

func (bridge *replayIntegrationBridge) Start(ctx context.Context, sandbox runner.Sandbox) (core.ToolEndpoint, error) {
	if sandbox != bridge.sandbox {
		return core.ToolEndpoint{}, errors.New("bridge received another sandbox")
	}
	return bridge.manager.StartTarget(ctx, bridge.sandbox.target)
}
func (bridge *replayIntegrationBridge) Stop(ctx context.Context) error {
	bridge.stops++
	return bridge.manager.Stop(ctx)
}

type replayIntegrationCaller struct {
	t         *testing.T
	client    sandboxv1.SandboxClient
	stops     int
	started   bool
	stoppedAt time.Time
}

func (caller *replayIntegrationCaller) Start(_ context.Context, endpoint core.ToolEndpoint, _ core.HarnessConnectivity, _, _ string) error {
	caller.started = true
	caller.client = dial(caller.t, endpoint)
	return nil
}
func (caller *replayIntegrationCaller) Run(ctx context.Context, plan replay.Plan) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return ReplayCalls(ctx, caller.client, plan)
}
func (caller *replayIntegrationCaller) Stop(context.Context) error {
	caller.stops++
	caller.stoppedAt = time.Now()
	return nil
}

func captureReplayIntegrationSource(t *testing.T) string {
	t.Helper()
	sourceTarget := newFakeExecutor(fstest.MapFS{"app/f": &fstest.MapFile{Data: []byte("x\n"), Mode: 0o600}})
	sourceTarget.stdout = []byte(replayIntegrationSourceOutput)
	manager, client, endpoint := startBridge(t, sourceTarget, Options{CaptureReplay: true, OmitRawLog: true})
	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "true"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(replayIntegrationWait)
	if _, err := client.Stat(context.Background(), &sandboxv1.StatRequest{Path: "/app/f"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(replayIntegrationWait)
	if _, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/f"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(replayIntegrationWait)
	const firstLine = 1
	const oneLine = 1
	if _, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/f", FirstLine: firstLine, MaxLines: oneLine}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(replayIntegrationWait)
	payload := []byte("x")
	if _, err := writeFile(client, "/app/f", int64(len(payload)), payload); err != nil {
		t.Fatal(err)
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
	finished, err := time.Parse(time.RFC3339Nano, records[len(records)-1].Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	return writeReplaySource(t, records, finished.Add(replayIntegrationWait), core.Evaluation{Status: core.StatusSucceeded, Score: replayIntegrationScore, Reward: replayIntegrationReward})
}

func TestReplayExecDefaultAuditReportsSameLengthDrift(t *testing.T) {
	const sourceOutput = "same one"
	const replayOutput = "same two"
	sourceTarget := newFakeExecutor(nil)
	sourceTarget.stdout = []byte(sourceOutput)
	sourceTarget.stderr = []byte("source error")
	manager, client, endpoint := startBridge(t, sourceTarget, Options{CaptureReplay: true, OmitRawLog: true})
	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "true"}); err != nil {
		t.Fatal(err)
	}
	stop(t, manager)
	sourceRecords := readToolCalls(t, endpoint.LogPaths[0])
	if len(sourceRecords) != replayIntegrationCount || sourceRecords[0]["response_stdout"] != sourceOutput || sourceRecords[0]["response_stderr"] != "source error" {
		t.Fatalf("source audit = %+v", sourceRecords)
	}
	log, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record toolCallRecord
	if err := json.Unmarshal(bytes.TrimSpace(log), &record); err != nil {
		t.Fatal(err)
	}
	record.RunID, record.TaskID, record.SandboxID = replayRunID, replayTaskID, replaySandboxID
	stamped, err := time.Parse(time.RFC3339Nano, record.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	source := writeReplaySource(t, []toolCallRecord{record}, stamped.Add(replayIntegrationWait), core.Evaluation{Status: core.StatusSucceeded, Reward: replayIntegrationReward})
	if _, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions); err != nil {
		t.Fatal(err)
	}
	target := newFakeExecutor(nil)
	target.stdout = []byte(replayOutput)
	target.stderr = []byte("source error")
	sandbox := &replayIntegrationSandbox{target: target}
	bridge := &replayIntegrationBridge{manager: newTestManager(t, Options{CaptureReplay: true, OmitRawLog: true}), sandbox: sandbox}
	caller := &replayIntegrationCaller{t: t}
	var logs bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&logs)
	engine, err := runner.NewReplay(replayIntegrationBenchmark{replayTask}, sandbox, bridge, caller,
		func(context.Context) (replay.Plan, error) {
			return LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
		},
		runner.Options{RunID: "replay-run", Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := engine.Run(context.Background())
	if runErr != nil || result.Tasks[0].Replay.Status != core.StatusSucceeded || result.Tasks[0].Replay.Responses.Status != "available" || len(result.Tasks[0].Replay.Responses.Calls) != replayIntegrationCount || result.Tasks[0].Replay.Responses.Calls[0].Status != "drifted" {
		t.Fatalf("same-length output drift failed replay or was not reported: result=%+v error=%v", result, runErr)
	}
	if !bytes.Contains(logs.Bytes(), []byte(sourceOutput)) || !bytes.Contains(logs.Bytes(), []byte(replayOutput)) || !bytes.Contains(logs.Bytes(), []byte("expected_sha256")) || !bytes.Contains(logs.Bytes(), []byte("actual_sha256")) {
		t.Fatalf("drift output or hashes missing from private log: %s", logs.String())
	}
	replayed := readToolCalls(t, result.Tasks[0].ToolLogPaths[0])
	if len(replayed) != replayIntegrationCount || replayed[0]["response_stdout"] != replayOutput || replayed[0]["response_stderr"] != "source error" || replayed[0]["replay_source_sequence"] != sourceRecords[0]["sequence"] || replayed[0]["response_sha256"] == sourceRecords[0]["response_sha256"] || replayed[0]["response_bytes"] != sourceRecords[0]["response_bytes"] {
		t.Fatalf("replay audit = %+v, source = %+v", replayed, sourceRecords)
	}
}

func TestCaptureToReplayPreservesFiveCallsAndInheritedGrade(t *testing.T) {
	source := captureReplayIntegrationSource(t)
	plan, err := LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Calls) != len(callEveryRPCNames) || plan.FinalWait != replayIntegrationWait {
		t.Fatalf("recorded source plan = %+v", plan)
	}
	for index, call := range plan.Calls {
		if call.Operation != callEveryRPCNames[index] || index > 0 && call.StartOffset <= plan.Calls[index-1].StartOffset {
			t.Fatalf("source call %d = %+v", index, call)
		}
	}
	for _, test := range []struct {
		name, changedOutput string
		wantCalls           int
		strict, drift       bool
	}{
		{name: "matching trace", changedOutput: replayIntegrationSourceOutput, wantCalls: len(plan.Calls)},
		{name: "same-length drift", changedOutput: "drifts", wantCalls: len(plan.Calls), drift: true},
		{name: "strict response length", changedOutput: "changed", wantCalls: replayIntegrationCount, strict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := newFakeExecutor(fstest.MapFS{"app/f": &fstest.MapFile{Data: []byte("x\n"), Mode: 0o600}})
			target.stdout = []byte(test.changedOutput)
			sandbox := &replayIntegrationSandbox{target: target}
			bridge := &replayIntegrationBridge{manager: newTestManager(t, Options{CaptureReplay: true, OmitRawLog: true}), sandbox: sandbox}
			caller := &replayIntegrationCaller{t: t}
			engine, err := runner.NewReplay(replayIntegrationBenchmark{replayTask}, sandbox, bridge, caller,
				func(context.Context) (replay.Plan, error) {
					return LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
				},
				runner.Options{RunID: "replay-run"})
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := engine.Run(context.Background())
			if test.strict && (runErr == nil || !bytes.Contains([]byte(runErr.Error()), []byte("mismatch"))) || !test.strict && runErr != nil {
				t.Fatalf("replay error = %v", runErr)
			}
			if len(result.Tasks) != replayIntegrationCount {
				t.Fatalf("task occurrences = %d", len(result.Tasks))
			}
			task := result.Tasks[0]
			if task.Isolation.Status != core.StatusConfirmed || !task.Isolation.CallerStopped || !task.Isolation.BridgeRevoked || task.Cleanup.Status != core.StatusSucceeded || !task.Evaluation.Reused || task.Evaluation.Status != core.StatusSucceeded || task.Evaluation.SourceRunID != replayRunID || task.Evaluation.SourceTaskID != replayTaskID || task.Evaluation.Score != replayIntegrationScore || task.Evaluation.Reward != replayIntegrationReward || result.Summary.EvaluationsRun != 0 {
				t.Fatalf("replay result = %+v", result)
			}
			if test.strict && (task.Replay.Status != core.StatusFailed || task.Replay.Error == "") || !test.strict && task.Replay.Status != core.StatusSucceeded {
				t.Fatalf("replay outcome = %+v", task.Replay)
			}
			if test.drift && (task.Replay.Responses == nil || task.Replay.Responses.Status != "available" || len(task.Replay.Responses.Calls) != len(plan.Calls) || task.Replay.Responses.Calls[0].Status != "drifted") {
				t.Fatalf("later calls did not finish after hash drift: %+v", task.Replay.Responses)
			}
			if test.strict && (task.Replay.Timing == nil || task.Replay.Timing.Status != "unavailable" || task.Replay.Timing.Reason == "") {
				t.Fatalf("mismatch hid missing timing: %+v", task.Replay)
			}
			if !test.strict {
				if len(task.ToolLogPaths) == 0 {
					t.Fatal("replay bridge audit is missing")
				}
				actual := readToolCalls(t, task.ToolLogPaths[0])
				if len(actual) != len(plan.Calls) {
					t.Fatalf("replay audit has %d calls, want %d", len(actual), len(plan.Calls))
				}
				firstStart := recordStartedAt(t, actual[0])
				const timingTolerance = replayIntegrationWait / 2
				if task.Replay.Timing == nil || task.Replay.Timing.Status != "available" || len(task.Replay.Timing.Calls) != len(plan.Calls) {
					t.Fatalf("replay timing report = %+v", task.Replay.Timing)
				}
				for index, record := range actual {
					if record["replay_source_sequence"] != float64(plan.Calls[index].Sequence) {
						t.Fatalf("call %d lost source sequence: %v", index, record["replay_source_sequence"])
					}
					offset := recordStartedAt(t, record).Sub(firstStart)
					drift := offset - plan.Calls[index].StartOffset
					if task.Replay.Timing.Calls[index].SourceSequence != plan.Calls[index].Sequence || task.Replay.Timing.Calls[index].DriftNS != drift.Nanoseconds() {
						t.Fatalf("call %d timing report = %+v, want drift %v", index, task.Replay.Timing.Calls[index], drift)
					}
					if drift < -timingTolerance || drift > timingTolerance {
						t.Fatalf("call %d start drift = %v, want within %v", index, drift, timingTolerance)
					}
				}
				lastCompleted, err := time.Parse(time.RFC3339Nano, actual[len(actual)-1]["timestamp"].(string))
				if err != nil {
					t.Fatal(err)
				}
				wait := caller.stoppedAt.Sub(lastCompleted)
				if wait < plan.FinalWait || wait > plan.FinalWait+timingTolerance {
					t.Fatalf("post-call wait = %v, want %v within %v", wait, plan.FinalWait, timingTolerance)
				}
			}
			commands, _, fileOps := target.snapshot()
			if len(commands)+fileOps != test.wantCalls || sandbox.starts != replayIntegrationCount || sandbox.stops != replayIntegrationCount || bridge.stops != replayIntegrationCount || caller.stops != replayIntegrationCount || !caller.started {
				t.Fatalf("commands=%d files=%d sandbox=%+v bridge stops=%d caller=%+v", len(commands), fileOps, sandbox, bridge.stops, caller)
			}
		})
	}
}

var callEveryRPCNames = [...]string{"Exec", "Stat", "ReadFile", "ReadLines", "WriteFile"}

func TestUnreplayableEleventhCallPreventsSandboxCreation(t *testing.T) {
	const earlierCalls = 10
	const firstSequence = 1
	const callGap = time.Second
	const callDuration = time.Millisecond
	start, err := time.Parse(time.RFC3339Nano, replaySourceTime)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*toolCallRecord){
		"missing write body": func(record *toolCallRecord) {
			record.OperationClass, record.Path, record.StdinEncoding, record.ReplayError = kindFileWrite, "/app/f", "binary-omitted", "write-content-omitted"
		},
		"canceled call": func(record *toolCallRecord) { record.Status = "canceled" },
		"refused call":  func(record *toolCallRecord) { record.Status = "rejected" },
	} {
		t.Run(name, func(t *testing.T) {
			records := make([]toolCallRecord, earlierCalls+firstSequence)
			for index := range records {
				started := start.Add(time.Duration(index) * callGap)
				records[index] = replayRecord(started, started.Add(callDuration), uint64(index+firstSequence))
			}
			change(&records[earlierCalls])
			source := writeReplaySource(t, records, start.Add(time.Duration(len(records))*callGap), core.Evaluation{Status: core.StatusSucceeded, Reward: firstSequence})
			sandbox := &replayIntegrationSandbox{target: newFakeExecutor(nil)}
			bridge := &replayIntegrationBridge{manager: newTestManager(t, Options{CaptureReplay: true, OmitRawLog: true}), sandbox: sandbox}
			caller := &replayIntegrationCaller{t: t}
			engine, err := runner.NewReplay(replayIntegrationBenchmark{replayTask}, sandbox, bridge, caller,
				func(context.Context) (replay.Plan, error) {
					return LoadReplaySource(source, replayTask, replayBenchmark, replayRevisions)
				},
				runner.Options{RunID: "replay-run"})
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := engine.Run(context.Background())
			if runErr == nil || len(result.Tasks) != firstSequence || result.Tasks[0].Replay.Status != core.StatusFailed {
				t.Fatalf("invalid source accepted: result=%+v error=%v", result, runErr)
			}
			if sandbox.starts != 0 || bridge.stops != 0 || caller.started {
				t.Fatalf("invalid source created runtime: sandbox=%+v bridge=%+v caller=%+v", sandbox, bridge, caller)
			}
		})
	}
}
