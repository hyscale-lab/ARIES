package deepresearchbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/model"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// raceArtifact is what judge_response.json holds: the judge's raw
// structured output plus any criterion-weight-matching warnings, so a
// human reviewing artifacts can see both the score and how it was derived.
type raceArtifact struct {
	Overall              float64         `json:"overall"`
	Comprehensiveness    float64         `json:"comprehensiveness"`
	Insight              float64         `json:"insight"`
	InstructionFollowing float64         `json:"instruction_following"`
	Readability          float64         `json:"readability"`
	Raw                  raceJudgeOutput `json:"raw"`
	Warnings             []string        `json:"warnings,omitempty"`
}

// Evaluate downloads the agent's report from the fixed report path after both
// isolation gates, then — unless the judge is disabled (Options.JudgeDisabled) —
// grades it against the pinned reference report using the RACE algorithm,
// and, if FACT is configured, verifies its citations. Grading itself is
// host-side: unlike Terminal-Bench 2, no code runs inside the sandbox during
// evaluation.
//
// evaluation.Score is RACE's Overall ratio (target/(target+reference))
// scaled to 0-100 for continuity with the existing reward-threshold
// contract: Score >= rewardThreshold (default 50) means the candidate tied
// or beat the reference article, NOT "scored 50% on an absolute rubric."
// FACT is purely additive: its result (or failure) never affects Score,
// Reward, or Status, which are determined by RACE alone. When the judge is
// disabled, no LLM grading happens at all: Status and VerifierStatus become
// core.StatusNotEnabled (the same "intentionally off" signal used elsewhere
// for a disabled Observer) and Score/Reward are 0, distinct from a run that
// was graded and failed.
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
		return finish(errors.New("deepresearchbench evaluator requires a live sandbox"))
	}
	b.mu.RLock()
	numericID, ok := b.numericIDs[task.ID]
	b.mu.RUnlock()
	if !ok {
		return finish(fmt.Errorf("deepresearchbench task %q was not loaded by Tasks", task.ID))
	}
	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return finish(fmt.Errorf("reverify deepresearchbench checkout before evaluation: %w", err))
	}

	artifactDir := filepath.Join(b.outputDir, task.ID, "evaluation")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return finish(fmt.Errorf("create evaluator artifact directory: %w", err))
	}
	reportArtifactPath := filepath.Join(artifactDir, "report.md")
	promptArtifactPath := filepath.Join(artifactDir, "judge_prompt.txt")
	responseArtifactPath := filepath.Join(artifactDir, "judge_response.json")
	factArtifactPath := filepath.Join(artifactDir, "fact_report.json")
	factErrorArtifactPath := filepath.Join(artifactDir, "fact_error.txt")
	for _, path := range []string{reportArtifactPath, promptArtifactPath, responseArtifactPath, factArtifactPath, factErrorArtifactPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return finish(fmt.Errorf("remove stale evaluator artifact %q: %w", path, err))
		}
	}
	evaluation.LogPaths = []string{reportArtifactPath}

	// A missing report is a legitimate (bad) task outcome, not a plumbing
	// failure: an agent that times out or gives up mid-research never writes
	// the file. Score it as a fail without surfacing a Go error.
	if err := sandbox.Download(ctx, reportPath, reportArtifactPath); err != nil {
		evaluation.Score = 0
		evaluation.Reward = 0
		evaluation.Error = fmt.Sprintf("agent did not produce a report at %s: %v", reportPath, err)
		evaluation.Duration = time.Since(started)
		return evaluation, nil
	}

	reportBytes, err := os.ReadFile(reportArtifactPath)
	if err != nil {
		return finish(fmt.Errorf("read downloaded report: %w", err))
	}
	report := string(reportBytes)

	// RACE and FACT are independent LLM workloads over the same report; run
	// them concurrently when both are on. A FACT failure is captured
	// separately and never propagated as Evaluate's error — only RACE
	// determines pass/fail, and only when it's enabled at all.
	var raceResultValue raceResult
	var raceErr error
	var factResultValue factReport
	var factErr error
	var wg sync.WaitGroup

	if b.race != nil {
		references, err := loadReferenceArticles(filepath.Join(b.root, b.referenceFile))
		if err != nil {
			return finish(fmt.Errorf("load deepresearchbench reference articles: %w", err))
		}
		reference, ok := references[numericID]
		if !ok {
			return finish(fmt.Errorf("no reference article for deepresearchbench task %q", task.ID))
		}
		prompts, err := loadPrompts(filepath.Join(b.root, b.queryFile))
		if err != nil {
			return finish(fmt.Errorf("load deepresearchbench prompts: %w", err))
		}
		prompt, ok := prompts[numericID]
		if !ok {
			return finish(fmt.Errorf("no prompt for deepresearchbench task %q", task.ID))
		}
		criteria, err := loadCriteria(filepath.Join(b.root, b.criteriaFile))
		if err != nil {
			return finish(fmt.Errorf("load deepresearchbench criteria: %w", err))
		}
		taskRubric, ok := criteria[numericID]
		if !ok {
			return finish(fmt.Errorf("no criteria for deepresearchbench task %q", task.ID))
		}

		if err := os.WriteFile(promptArtifactPath, []byte(prompt), 0o600); err != nil {
			return finish(fmt.Errorf("write judge prompt artifact: %w", err))
		}
		evaluation.LogPaths = append(evaluation.LogPaths, promptArtifactPath, responseArtifactPath)

		wg.Add(1)
		go func() {
			defer wg.Done()
			raceResultValue, raceErr = b.race.Score(ctx, prompt, report, reference, taskRubric)
		}()
	}
	if b.fact != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			factResultValue, factErr = b.fact.Run(ctx, report)
		}()
	}
	wg.Wait()

	if b.race != nil {
		if raceErr != nil {
			return finish(fmt.Errorf("race-score deepresearchbench report: %w", raceErr))
		}

		artifact := raceArtifact{
			Overall:              raceResultValue.Overall,
			Comprehensiveness:    raceResultValue.Comprehensiveness,
			Insight:              raceResultValue.Insight,
			InstructionFollowing: raceResultValue.InstructionFollowing,
			Readability:          raceResultValue.Readability,
			Raw:                  raceResultValue.Raw,
			Warnings:             raceResultValue.Warnings,
		}
		responseBytes, err := json.MarshalIndent(artifact, "", "  ")
		if err != nil {
			return finish(fmt.Errorf("encode judge response artifact: %w", err))
		}
		if err := os.WriteFile(responseArtifactPath, responseBytes, 0o600); err != nil {
			return finish(fmt.Errorf("write judge response artifact: %w", err))
		}
	}

	// RACE's sub-dimension breakdown lives only in the judge_response.json
	// artifact above (see raceArtifact); core.Evaluation carries just the
	// single Score/Reward pair, so it stays unchanged for benchmarks other
	// than Deep Research Bench.
	if b.fact != nil {
		if factErr != nil {
			if writeErr := os.WriteFile(factErrorArtifactPath, []byte(factErr.Error()), 0o600); writeErr == nil {
				evaluation.LogPaths = append(evaluation.LogPaths, factErrorArtifactPath)
			}
		} else {
			factBytes, err := json.MarshalIndent(factResultValue, "", "  ")
			if err == nil {
				if writeErr := os.WriteFile(factArtifactPath, factBytes, 0o600); writeErr == nil {
					evaluation.LogPaths = append(evaluation.LogPaths, factArtifactPath)
				}
			}
		}
	}

	if b.race != nil {
		evaluation.Score = raceResultValue.Overall * 100.0
		if evaluation.Score >= b.rewardThreshold {
			evaluation.Reward = 1
			evaluation.Status = core.StatusSucceeded
			evaluation.VerifierStatus = core.StatusSucceeded
		} else {
			evaluation.Reward = 0
		}
	} else {
		evaluation.Score = 0
		evaluation.Reward = 0
		evaluation.Status = core.StatusNotEnabled
		evaluation.VerifierStatus = core.StatusNotEnabled
	}
	return finish(nil)
}

// chatter keeps benchmark response parsing and retry policy independently testable.
type chatter interface {
	Chat(context.Context, string, string) (string, error)
}

func newJudgeClient(settings core.ModelConfig, lookup func(string) ([]byte, bool)) (*model.ChatClient, error) {
	if settings.Temperature == nil && (settings.ReasoningEffort == "" || settings.ReasoningEffort == "off" || settings.ReasoningEffort == "none") {
		temperature := 0.0
		settings.Temperature = &temperature
	}
	return model.NewChatClient(settings, lookup)
}

// stripJSONCodeFence removes a surrounding ```json ... ``` or ``` ... ```
// markdown fence, if present, mirroring upstream's
// `response.replace("```json", "").replace("```", "")` cleanup applied
// before every JSON parse in the Python pipeline. Content without a fence
// is returned unchanged.
func stripJSONCodeFence(content string) string {
	trimmed := strings.TrimSpace(content)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	return strings.TrimSpace(trimmed)
}
