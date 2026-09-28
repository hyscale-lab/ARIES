package roadmapbench

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// Evaluate runs the pinned official verifier after the Runner's two isolation
// gates. RoadmapBench rewards partial completion; only a reward of 1 resolves
// a task. Per-task reward.json schemas differ and are retained without rewriting.
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
		return finish(errors.New("roadmapbench evaluation requires a live sandbox"))
	}
	b.mu.RLock()
	details, loaded := b.details[task.ID]
	b.mu.RUnlock()
	if !loaded {
		return finish(fmt.Errorf("roadmapbench task %q was not loaded by Tasks", task.ID))
	}
	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return finish(fmt.Errorf("reverify roadmapbench checkout before evaluation: %w", err))
	}
	downloader, ok := sandbox.(runner.LimitedDownloader)
	if !ok {
		return finish(errors.New("roadmapbench evaluation requires bounded sandbox downloads"))
	}
	artifactDir := filepath.Join(b.outputDir, task.ID, "evaluation")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return finish(fmt.Errorf("create evaluation directory: %w", err))
	}
	stdoutPath := filepath.Join(artifactDir, "stdout.log")
	stderrPath := filepath.Join(artifactDir, "stderr.log")
	rewardPath := filepath.Join(artifactDir, "reward.txt")
	detailsPath := filepath.Join(artifactDir, "reward.json")
	for _, file := range []string{stdoutPath, stderrPath, rewardPath, detailsPath} {
		if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return finish(fmt.Errorf("remove stale evaluator artifact: %w", err))
		}
	}
	resetPaths := append([]string{testsPath, verifierLogPath}, verifierScratchPaths(details.taskID)...)
	if err := execChecked(ctx, sandbox, core.Command{
		Path: "/bin/rm", Args: append([]string{"-rf", "--"}, resetPaths...), User: "0:0",
	}); err != nil {
		return finish(fmt.Errorf("remove stale verifier paths: %w", err))
	}
	if err := execChecked(ctx, sandbox, core.Command{
		Path: "/bin/sh", Args: append([]string{"-c", absencePredicate, "aries-roadmapbench-absence"}, resetPaths...), User: "0:0",
	}); err != nil {
		return finish(fmt.Errorf("confirm stale verifier paths absent: %w", err))
	}
	directories := []string{testsPath, verifierLogPath}
	seen := map[string]bool{testsPath: true, verifierLogPath: true}
	for _, file := range details.verifierFiles {
		for dir := path.Dir(file.destination); dir != testsPath; dir = path.Dir(dir) {
			if !seen[dir] {
				seen[dir] = true
				directories = append(directories, dir)
			}
		}
	}
	slices.Sort(directories[2:])
	if err := execChecked(ctx, sandbox, core.Command{
		Path: "/bin/mkdir", Args: append([]string{"-p", "--"}, directories...), User: "0:0",
	}); err != nil {
		return finish(fmt.Errorf("create verifier directories: %w", err))
	}
	for _, file := range details.verifierFiles {
		if err := sandbox.Upload(ctx, file.source, file.destination); err != nil {
			return finish(fmt.Errorf("inject private verifier %q: %w", file.name, err))
		}
	}
	verifierEnv := cloneMap(details.verifierEnv)
	if details.taskID == "opt-3.0.0-roadmap" {
		if verifierEnv == nil {
			verifierEnv = make(map[string]string)
		}
		verifierEnv["TMPDIR"] = "/tmp"
	}
	result, runErr := sandbox.Exec(ctx, core.Command{
		Path: "/bin/bash", Args: []string{path.Join(testsPath, "test.sh")},
		Dir: details.workdir, Env: verifierEnv, Timeout: details.timeout,
		User: "0:0", OutputLimitBytes: 32 << 20,
	})
	var failures []error
	for _, log := range []struct{ file, content string }{{stdoutPath, result.Stdout}, {stderrPath, result.Stderr}} {
		if err := os.WriteFile(log.file, []byte(log.content), 0o600); err != nil {
			failures = append(failures, fmt.Errorf("write verifier log: %w", err))
		} else {
			evaluation.LogPaths = append(evaluation.LogPaths, log.file)
		}
	}
	if err := downloader.DownloadLimit(ctx, path.Join(verifierLogPath, "reward.txt"), rewardPath, 1024); err != nil {
		failures = append(failures, fmt.Errorf("download verifier reward: %w", err))
	} else {
		evaluation.LogPaths = append(evaluation.LogPaths, rewardPath)
	}
	if err := downloader.DownloadLimit(ctx, path.Join(verifierLogPath, "reward.json"), detailsPath, 16<<20); err != nil {
		if !errors.Is(err, runner.ErrNotFound) {
			failures = append(failures, fmt.Errorf("download verifier details: %w", err))
		}
	} else {
		evaluation.LogPaths = append(evaluation.LogPaths, detailsPath)
	}
	if runErr != nil {
		failures = append(failures, fmt.Errorf("run verifier: %w", runErr))
	}
	if result.ExitCode != 0 {
		failures = append(failures, fmt.Errorf("run verifier: exit code %d", result.ExitCode))
	}
	if len(failures) != 0 {
		return finish(errors.Join(failures...))
	}
	contents, err := os.ReadFile(rewardPath)
	if err != nil {
		return finish(fmt.Errorf("read reward: %w", err))
	}
	reward, err := strconv.ParseFloat(strings.TrimSpace(string(contents)), 64)
	if err != nil || math.IsNaN(reward) || math.IsInf(reward, 0) || reward < 0 || reward > 1 {
		return finish(errors.New("malformed RoadmapBench reward: expected a finite number in [0,1]"))
	}
	evaluation.Score, evaluation.Reward = reward, reward
	if reward == 1 {
		evaluation.Status, evaluation.VerifierStatus = core.StatusSucceeded, core.StatusSucceeded
	}
	return finish(nil)
}
