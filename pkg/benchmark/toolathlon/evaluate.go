package toolathlon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

var evalTimeout = 15 * time.Minute

// trajectoryStub is the trajectory file Toolathlon's container_eval reads.
// Its grader trusts nothing in it: the task config is replaced from the
// bundle preserved on the host, and the status from --agent_exit_code. What
// it grades is the sandbox state — the workspace files and the self-hosted
// applications — which is the ARIES evaluation contract. No pinned grader
// reads the messages, so an empty list is the honest value: the harness's
// own trajectory is a separate ARIES artifact.
const trajectoryStub = `{"config": {}, "status": "success", "messages": [], "tool_calls": {},` +
	` "aries": "trajectory stub written by the ARIES Toolathlon adapter; the agent ran in the harness"}` + "\n"

// evalResult is Toolathlon's eval_res.json.
type evalResult struct {
	Pass    *bool  `json:"pass"`
	Details string `json:"details"`
	Failure string `json:"failure"`
}

// Evaluate restores the grader, re-injects the trusted bundle, and runs
// Toolathlon's container_eval against the sandbox the agent left behind.
// The gateway and the loopback forwarder are still running: graders for
// application-backed tasks query the applications the same way preprocess
// seeded them.
func (b *Benchmark) Evaluate(ctx context.Context, task core.Task, sandbox runner.Sandbox) (core.Evaluation, error) {
	started := time.Now()
	evaluation := core.Evaluation{Status: core.StatusFailed, VerifierStatus: core.StatusFailed}
	finish := func(err error) (core.Evaluation, error) {
		evaluation.Duration = time.Since(started)
		if err != nil {
			evaluation.Error = err.Error()
		}
		return evaluation, err
	}

	if sandbox == nil {
		return finish(errors.New("toolathlon evaluator requires a live sandbox"))
	}
	b.mu.RLock()
	details, ok := b.details[task.ID]
	b.mu.RUnlock()
	if !ok {
		return finish(fmt.Errorf("toolathlon task %q was not loaded by Tasks", task.ID))
	}
	if details.stashed == nil {
		return finish(fmt.Errorf("toolathlon task %q was not prepared", task.ID))
	}
	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return finish(fmt.Errorf("reverify toolathlon checkout before evaluation: %w", err))
	}

	hostDir := filepath.Join(b.outputDir, task.ID, "toolathlon")
	bundleHostPath := filepath.Join(hostDir, "task_bundle.json")
	stashHostPath := filepath.Join(hostDir, "artifact-stash.tar")
	evalLogPath := filepath.Join(hostDir, "eval.log")
	evalResultHostPath := filepath.Join(hostDir, "eval_res.json")
	gatewayLogHostPath := filepath.Join(hostDir, "gateway.log")
	evaluation.LogPaths = []string{evalLogPath, evalResultHostPath}
	for _, path := range []string{evalLogPath, evalResultHostPath, gatewayLogHostPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return finish(fmt.Errorf("remove stale evaluator artifact %q: %w", path, err))
		}
	}
	for _, path := range []string{bundleHostPath, stashHostPath} {
		if _, err := os.Stat(path); err != nil {
			return finish(fmt.Errorf("trusted preparation artifact missing: %w", err))
		}
	}

	// The gateway log is telemetry, not evidence; its absence is not an
	// evaluation failure.
	if err := sandbox.Download(ctx, gatewayLogPath, gatewayLogHostPath); err == nil {
		evaluation.LogPaths = append(evaluation.LogPaths, gatewayLogHostPath)
	}

	if err := restoreArtifacts(ctx, sandbox, details.name, stashHostPath, details.stashed); err != nil {
		return finish(fmt.Errorf("restore grader artifacts: %w", err))
	}
	// Anything the agent left at the two paths the grader reads is discarded.
	if err := removePaths(ctx, sandbox, []string{trajectoryPath, evalResultPath, bundleContainerPath}); err != nil {
		return finish(fmt.Errorf("clear grader inputs: %w", err))
	}
	stubHostPath := filepath.Join(hostDir, "traj_log.json")
	if err := os.WriteFile(stubHostPath, []byte(trajectoryStub), 0o600); err != nil {
		return finish(fmt.Errorf("write trajectory stub: %w", err))
	}
	if err := sandbox.Upload(ctx, stubHostPath, trajectoryPath); err != nil {
		return finish(fmt.Errorf("inject trajectory stub: %w", err))
	}
	if err := sandbox.Upload(ctx, bundleHostPath, bundleContainerPath); err != nil {
		return finish(fmt.Errorf("inject trusted task bundle: %w", err))
	}

	command := uvCommand(
		"run", "python", "-m", "scripts.decoupled.container_eval",
		"--bundle_file", bundleContainerPath,
		"--require_resolved_task_config",
		"--consume_bundle",
		"--agent_exit_code", "0",
	)
	command.Timeout = evalTimeout
	result, execErr := sandbox.Exec(ctx, command)
	var artifactErrors []error
	if err := os.WriteFile(evalLogPath, []byte(result.Stdout+result.Stderr), 0o600); err != nil {
		artifactErrors = append(artifactErrors, fmt.Errorf("write evaluator log: %w", err))
	}
	if err := sandbox.Download(ctx, evalResultPath, evalResultHostPath); err != nil {
		artifactErrors = append(artifactErrors, fmt.Errorf("download evaluator result: %w", err))
	}
	if execErr != nil {
		artifactErrors = append(artifactErrors, fmt.Errorf("run toolathlon evaluator: %w", execErr))
	}
	if len(artifactErrors) != 0 {
		return finish(errors.Join(artifactErrors...))
	}
	// container_eval exits 1 for a graded failure as well as for a crash;
	// the result file, not the exit code, is the verdict.
	parsed, err := parseEvalResultFile(evalResultHostPath)
	if err != nil {
		return finish(fmt.Errorf("%w (evaluator exit code %d)", err, result.ExitCode))
	}
	if parsed.Pass == nil {
		return finish(fmt.Errorf("toolathlon evaluator did not grade the task: %s", strings.TrimSpace(parsed.Details)))
	}
	if *parsed.Pass {
		evaluation.Score = 1
		evaluation.Reward = 1
		evaluation.Status = core.StatusSucceeded
		evaluation.VerifierStatus = core.StatusSucceeded
	}
	return finish(nil)
}

func parseEvalResultFile(path string) (evalResult, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return evalResult{}, fmt.Errorf("read evaluator result: %w", err)
	}
	return parseEvalResult(content)
}

func parseEvalResult(content []byte) (evalResult, error) {
	var parsed evalResult
	if err := json.Unmarshal(content, &parsed); err != nil {
		return evalResult{}, fmt.Errorf("parse evaluator result: %w", err)
	}
	return parsed, nil
}
