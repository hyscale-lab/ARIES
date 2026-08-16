package sweatlas

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

// finalAnswerTag is the marker instruction.md tells the agent to wrap its
// final answer in, mirroring evaluate_answer.py's own "<<FINAL_ANSWER>>"
// split.
const finalAnswerTag = "<<FINAL_ANSWER>>"

// Evaluate downloads the agent's answer file from the still-live sandbox,
// then — unless it's missing or empty — grades it host-side against the
// task's rubric using an LLM judge, exactly like Deep Research Bench's RACE
// grading: no code runs inside the sandbox during evaluation, only one file
// download.
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
		return finish(errors.New("sweatlas evaluator requires a live sandbox"))
	}
	b.mu.RLock()
	details, ok := b.details[task.ID]
	b.mu.RUnlock()
	if !ok {
		return finish(fmt.Errorf("sweatlas task %q was not loaded by Tasks", task.ID))
	}
	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return finish(fmt.Errorf("reverify sweatlas checkout before evaluation: %w", err))
	}

	artifactDir := filepath.Join(b.outputDir, task.ID, "evaluation")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return finish(fmt.Errorf("create evaluator artifact directory: %w", err))
	}
	answerArtifactPath := filepath.Join(artifactDir, "answer.txt")
	rewardPath := filepath.Join(artifactDir, "reward.txt")
	resultsPath := filepath.Join(artifactDir, "evaluation_results.json")
	evaluation.LogPaths = []string{answerArtifactPath, rewardPath, resultsPath}
	for _, path := range evaluation.LogPaths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return finish(fmt.Errorf("remove stale evaluator artifact %q: %w", path, err))
		}
	}

	// A missing answer file is a legitimate (bad) task outcome, not a
	// plumbing failure: an agent that times out or gives up mid-task never
	// writes it. Score it as a fail without surfacing a Go error, exactly
	// like evaluate_answer.py's own "no answer file" early return.
	if err := sandbox.Download(ctx, answerPath, answerArtifactPath); err != nil {
		return scoreNoAnswer(evaluation, started, rewardPath)
	}

	answerBytes, err := os.ReadFile(answerArtifactPath)
	if err != nil {
		return finish(fmt.Errorf("read downloaded answer: %w", err))
	}
	answer := extractFinalAnswer(string(answerBytes))
	if answer == "" {
		return scoreNoAnswer(evaluation, started, rewardPath)
	}

	rubrics, err := loadRubrics(filepath.Join(details.testsDir, "rubrics.json"))
	if err != nil {
		return finish(err)
	}
	systemPromptBytes, err := os.ReadFile(filepath.Join(details.testsDir, "system_prompt.txt"))
	if err != nil {
		return finish(fmt.Errorf("read system prompt: %w", err))
	}
	userPromptTemplateBytes, err := os.ReadFile(filepath.Join(details.testsDir, "user_prompt_template.txt"))
	if err != nil {
		return finish(fmt.Errorf("read user prompt template: %w", err))
	}
	problemStatement := ""
	if promptBytes, err := os.ReadFile(filepath.Join(details.testsDir, "prompt.txt")); err == nil {
		problemStatement = strings.TrimSpace(string(promptBytes))
	} else if !errors.Is(err, os.ErrNotExist) {
		return finish(fmt.Errorf("read problem statement: %w", err))
	}

	evalCtx := ctx
	if details.timeout > 0 {
		var cancel context.CancelFunc
		evalCtx, cancel = context.WithTimeout(ctx, details.timeout)
		defer cancel()
	}

	scores := make([]*canonicalResult, len(rubrics))
	rubricErrs := make([]error, len(rubrics))
	unscoredCount := 0
	for index, r := range rubrics {
		scores[index], rubricErrs[index] = evaluateSingleRubric(evalCtx, b.judge, string(systemPromptBytes), string(userPromptTemplateBytes), problemStatement, answer, r)
		if scores[index] == nil {
			unscoredCount++
		}
	}
	results := aggregateRubricResults(rubrics, scores)

	// Every unscored rubric has a non-nil rubricErrs entry (evaluateSingleRubric
	// never returns (nil, nil)); write them out so a systemic judge failure —
	// e.g. every call getting a 401, or the judge model never returning a
	// parsable rating — is diagnosable from the run's artifacts instead of
	// silently looking identical to a legitimately-scored 0.
	if unscoredCount > 0 {
		var diagnostics strings.Builder
		for index, r := range rubrics {
			if rubricErrs[index] != nil {
				fmt.Fprintf(&diagnostics, "rubric %s (%s): %v\n", r.ID, r.Title, rubricErrs[index])
			}
		}
		errorsPath := filepath.Join(artifactDir, "judge_errors.log")
		if err := os.WriteFile(errorsPath, []byte(diagnostics.String()), 0o600); err == nil {
			evaluation.LogPaths = append(evaluation.LogPaths, errorsPath)
		}
		if unscoredCount == len(rubrics) {
			evaluation.Error = fmt.Sprintf("all %d rubrics could not be judged; see judge_errors.log", len(rubrics))
		}
	}

	if err := os.WriteFile(rewardPath, []byte(fmt.Sprintf("%d\n", results.Reward)), 0o600); err != nil {
		return finish(fmt.Errorf("write verifier reward: %w", err))
	}
	resultsBytes, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return finish(fmt.Errorf("encode verifier evaluation results: %w", err))
	}
	if err := os.WriteFile(resultsPath, resultsBytes, 0o600); err != nil {
		return finish(fmt.Errorf("write verifier evaluation results: %w", err))
	}

	evaluation.Reward = float64(results.Reward)
	evaluation.Score = results.AggScore
	if results.Reward == 1 {
		evaluation.Status = core.StatusSucceeded
		evaluation.VerifierStatus = core.StatusSucceeded
	}
	return finish(nil)
}

// scoreNoAnswer mirrors evaluate_answer.py's own "no answer file" / "empty
// answer" early return: only reward.txt is written (as "0"), no
// evaluation_results.json, and no Go error is surfaced — a missing or empty
// answer is a legitimate task outcome, not a plumbing fault.
func scoreNoAnswer(evaluation core.Evaluation, started time.Time, rewardPath string) (core.Evaluation, error) {
	if err := os.WriteFile(rewardPath, []byte("0\n"), 0o600); err != nil {
		evaluation.Duration = time.Since(started)
		evaluation.Error = fmt.Sprintf("write verifier reward: %v", err)
		return evaluation, err
	}
	evaluation.Reward = 0
	evaluation.Score = 0
	evaluation.Duration = time.Since(started)
	return evaluation, nil
}

// extractFinalAnswer mirrors evaluate_answer.py's answer extraction: read
// the file, trim whitespace, and — if the <<FINAL_ANSWER>> tag is present —
// keep only the content after its first occurrence.
func extractFinalAnswer(content string) string {
	answer := strings.TrimSpace(content)
	if strings.Contains(answer, finalAnswerTag) {
		parts := strings.SplitN(answer, finalAnswerTag, 2)
		if len(parts) == 2 {
			answer = strings.TrimSpace(parts[1])
		}
	}
	return answer
}
