package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	bridgegrpc "github.com/hyscale-lab/aries/pkg/bridge/grpc"
	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/runner"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type workloadReplaySandbox struct {
	launchSandbox
	present bool
}

func (s *workloadReplaySandbox) Start(context.Context, core.SandboxRequest) (runner.Sandbox, error) {
	s.present = true
	return s, nil
}
func (s *workloadReplaySandbox) Stop(context.Context, runner.Sandbox) error {
	s.present = false
	return nil
}
func (*workloadReplaySandbox) Connectivity() core.HarnessConnectivity {
	return core.HarnessConnectivity{}
}

type workloadReplayBenchmark struct{ task core.Task }

func (b workloadReplayBenchmark) Tasks(context.Context) ([]core.Task, error) {
	return []core.Task{b.task}, nil
}
func (workloadReplayBenchmark) PrepareSandbox(context.Context, core.Task, runner.Sandbox) error {
	return nil
}
func (workloadReplayBenchmark) Evaluate(context.Context, core.Task, runner.Sandbox, runner.EvaluationSandboxes) (core.Evaluation, error) {
	panic("replay must not run a verifier or judge")
}

type workloadReplayCaller struct {
	connection *grpc.ClientConn
	origin     func() time.Time
}

func (c *workloadReplayCaller) Start(_ context.Context, endpoint core.ToolEndpoint, _ core.HarnessConnectivity, _, _ string) (err error) {
	c.connection, err = grpc.NewClient(endpoint.Address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
	return err
}
func (c *workloadReplayCaller) Run(ctx context.Context, plan replay.Plan) error {
	if len(plan.Calls) != 0 {
		origin := c.origin()
		plan.ReplayAt = &origin
	}
	return bridgegrpc.ReplayCalls(ctx, time.Minute, sandboxv1.NewSandboxClient(c.connection), plan)
}
func (c *workloadReplayCaller) Stop(context.Context) error {
	if c.connection == nil {
		return nil
	}
	err := c.connection.Close()
	c.connection = nil
	return err
}

func TestSessionCaptureToReplayWholeWorkload(t *testing.T) {
	const iterations = 2
	const callsPerOccurrence = 2
	const firstOccurrence = 1
	const image = "task-image:tag"
	const benchmark = "terminalbench2"
	const revision = "pinned-revision"
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			var logicalIDs []string
			for range iterations {
				logicalIDs = append(logicalIDs, "first", "second")
			}
			logicalIDs = append(logicalIDs, "empty")
			callCount := (len(logicalIDs) - firstOccurrence) * callsPerOccurrence
			if empty {
				callCount = 0
			}
			outputs := map[string]string{}
			for index, logicalID := range logicalIDs {
				id := fmt.Sprintf("%s-%03d", logicalID, index+firstOccurrence)
				for callIndex := range callsPerOccurrence {
					script := fmt.Sprintf("%s-call-%d", id, callIndex)
					outputs[script] = script + "\n"
				}
			}
			newService := func(runID string) (*Service, *sessionReplayRuntime, launchSandbox, chan struct{}) {
				t.Helper()
				service, base, sandbox := launchFixtureWith(t, "docker", "hermes-grpc", false)
				wait := make(chan struct{})
				runtime := &sessionReplayRuntime{launchRuntime: base, backend: sessionReplayBackend{commands: make(chan core.Command, callCount), wait: wait, outputs: outputs}}
				service.options.Runtime = runtime
				service.options.RunID = runID
				service.options.Launch.Config.OutputDir = t.TempDir()
				service.options.CaptureReplay = true
				service.options.BenchmarkType = benchmark
				service.options.BenchmarkRevisions = map[string]string{"revision": revision}
				sandbox.descriptor.RunID = runID
				if err := service.Start(ctx); err != nil {
					t.Fatal(err)
				}
				return service, runtime, sandbox, wait
			}
			releaseCalls := func(runtime *sessionReplayRuntime, wait chan struct{}) {
				t.Helper()
				// Every RPC must enter the backend before any RPC can complete.
				for range callCount {
					select {
					case <-runtime.backend.commands:
					case <-ctx.Done():
						t.Fatal("overlapping workload did not reach the backend", ctx.Err())
					}
				}
				close(wait)
			}
			sourceService, sourceRuntime, sourceSandbox, sourceWait := newService("source-run")
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			var sessions []*Session
			var connections []*grpc.ClientConn
			var tasks []core.Task
			sourceResult := core.RunResult{RunID: "source-run"}
			completed := make(chan error, callCount)
			for index, logicalID := range logicalIDs {
				id := fmt.Sprintf("%s-%03d", logicalID, index+firstOccurrence)
				sourceSandbox.descriptor.TaskID = id
				sourceSandbox.descriptor.SandboxID = "sandbox-" + id
				sourceSandbox.descriptor.RuntimeID = "runtime-" + id
				bridge, err := sourceService.NewSession(root)
				if err != nil {
					t.Fatal(err)
				}
				session := bridge.(*Session)
				session.RecordSource(logicalID, image)
				session.RecordTask(image)
				endpoint, err := session.Start(ctx, sourceSandbox)
				if err != nil {
					t.Fatal(err)
				}
				sessions = append(sessions, session)
				tasks = append(tasks, core.Task{ID: logicalID, Environment: core.Environment{Image: image, Workdir: endpoint.Workdir}})
				sourceResult.Tasks = append(sourceResult.Tasks, core.TaskResult{TaskID: id, Evaluation: core.Evaluation{Status: core.StatusSucceeded, Score: float64(index), VerifierStatus: id}})
				if empty || logicalID == "empty" {
					continue
				}
				connection, err := grpc.NewClient(endpoint.Address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
				if err != nil {
					t.Fatal(err)
				}
				connections = append(connections, connection)
				t.Cleanup(func() { _ = connection.Close() })
				for callIndex := range callsPerOccurrence {
					script := fmt.Sprintf("%s-call-%d", id, callIndex)
					go func() {
						_, err := sandboxv1.NewSandboxClient(connection).Exec(ctx, &sandboxv1.ExecRequest{Script: script})
						completed <- err
					}()
				}
			}
			releaseCalls(sourceRuntime, sourceWait)
			for range callCount {
				if err := <-completed; err != nil {
					t.Fatal(err)
				}
			}
			for _, connection := range connections {
				if err := connection.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for _, session := range sessions {
				if err := session.CompleteHarness(ctx); err != nil {
					t.Fatal(err)
				}
				if err := session.Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(sourceResult)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "run-result.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			original := map[string][]byte{}
			if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				original[path], err = os.ReadFile(path)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var catalog []core.Task
			for _, task := range tasks {
				if !slices.ContainsFunc(catalog, func(selected core.Task) bool { return selected.ID == task.ID }) {
					catalog = append(catalog, task)
				}
			}
			selection, err := bridgegrpc.PreflightReplaySource(ctx, root, catalog, benchmark, sourceService.options.BenchmarkRevisions, nil)
			if err != nil {
				t.Fatalf("preflight unchanged production capture: %v", err)
			}
			replayService, replayRuntime, sandbox, replayWait := newService("replay-run")
			origin := sync.OnceValue(func() time.Time { return time.Now().UTC() })
			results := make(chan error, len(selection.Occurrences))
			for index, occurrence := range selection.Occurrences {
				plan, err := selection.Load(ctx, index)
				if err != nil {
					t.Fatal(err)
				}
				sandbox.descriptor.TaskID = sourceResult.Tasks[index].TaskID
				sandbox.descriptor.SandboxID = "sandbox-" + sandbox.descriptor.TaskID
				sandbox.descriptor.RuntimeID = "runtime-" + sandbox.descriptor.TaskID
				owner := &workloadReplaySandbox{launchSandbox: sandbox}
				bridge, err := replayService.NewSession(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				caller := &workloadReplayCaller{origin: origin}
				task := core.Task{ID: owner.descriptor.TaskID, Environment: tasks[index].Environment}
				engine, err := runner.NewReplay(workloadReplayBenchmark{task}, owner, bridge, caller,
					func(context.Context) (replay.Plan, error) { return plan, nil }, runner.Options{RunID: "replay-run"})
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					result, err := engine.Run(ctx)
					if err == nil {
						got := result.Tasks[0]
						grade := sourceResult.Tasks[index].Evaluation
						grade.Reused, grade.SourceRunID, grade.SourceTaskID = true, sourceResult.RunID, occurrence.SourceTaskID
						if !reflect.DeepEqual(got.Evaluation, grade) || got.Replay.Status != core.StatusSucceeded || got.Isolation.Status != core.StatusConfirmed || !got.Isolation.CallerStopped || !got.Isolation.BridgeRevoked || got.Cleanup.Status != core.StatusSucceeded || owner.present || caller.connection != nil || result.Summary.EvaluationsRun != 0 {
							err = fmt.Errorf("replay lost source grade or cleanup: %+v", got)
						} else if got.Replay.Responses.Status != "available" || len(got.Replay.Responses.Calls) != len(plan.Calls) {
							err = fmt.Errorf("response correlation unavailable: %+v", got.Replay.Responses)
						} else {
							for callIndex, response := range got.Replay.Responses.Calls {
								if response.SourceSequence != plan.Calls[callIndex].Sequence || response.Status != "matched" {
									err = fmt.Errorf("wrong occurrence response: %+v", response)
								}
							}
						}
					}
					results <- err
				}()
			}
			releaseCalls(replayRuntime, replayWait)
			for range selection.Occurrences {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
			for path, before := range original {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("replay changed source %s: %v", path, err)
				}
			}
			if err := errors.Join(sourceService.Stop(ctx), replayService.Stop(ctx)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
