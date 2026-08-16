package sweatlas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

type evaluateFake struct {
	downloadErr     error
	downloadContent string
	downloadSource  string
	downloads       int
	uploads         int
}

func (s *evaluateFake) Exec(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, nil
}

func (s *evaluateFake) Upload(context.Context, string, string) error {
	s.uploads++
	return nil
}

func (s *evaluateFake) Download(_ context.Context, source, destination string) error {
	s.downloads++
	s.downloadSource = source
	if s.downloadErr != nil {
		return s.downloadErr
	}
	return os.WriteFile(destination, []byte(s.downloadContent), 0o600)
}

// stubChat is a chatter fake driven by a per-call response/error queue, in
// the order chat is invoked (across all rubrics and retries).
type stubChat struct {
	responses []string
	errs      []error
	calls     int
	prompts   []string
}

func (s *stubChat) chat(_ context.Context, _ string, userPrompt string) (string, error) {
	index := s.calls
	s.calls++
	s.prompts = append(s.prompts, userPrompt)
	var err error
	if index < len(s.errs) {
		err = s.errs[index]
	}
	if err != nil {
		return "", err
	}
	if index < len(s.responses) {
		return s.responses[index], nil
	}
	return "", errors.New("stubChat: no more canned responses")
}

func ratingResponse(status string) string {
	return fmt.Sprintf(`{"ratings":[{"rubric_statement":"stmt","status":%q,"justification":"j"}]}`, status)
}

func benchmarkWithFixtureAndChat(t *testing.T, chat chatter) (*Benchmark, core.Task) {
	t.Helper()
	root := writeFixture(t)
	task, details, err := loadTask(root, qaTaskID)
	if err != nil {
		t.Fatal(err)
	}
	benchmark, err := New(testOptions(root, []string{qaTaskID}, filepath.Join(t.TempDir(), "runs")))
	if err != nil {
		t.Fatal(err)
	}
	benchmark.details[qaTaskID] = details
	benchmark.judge = chat
	return benchmark, task
}

func TestEvaluateRequiresLiveSandbox(t *testing.T) {
	benchmark, task := benchmarkWithFixtureAndChat(t, &stubChat{})
	if _, err := benchmark.Evaluate(context.Background(), task, nil); err == nil {
		t.Fatal("Evaluate accepted a nil sandbox")
	}
}

func TestEvaluateMissingAnswerScoresZeroWithoutError(t *testing.T) {
	chat := &stubChat{}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadErr: errors.New("no such file")}

	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatalf("Evaluate returned an error for a missing answer: %v", err)
	}
	if evaluation.Status != core.StatusFailed || evaluation.Reward != 0 || evaluation.Score != 0 {
		t.Fatalf("evaluation = %#v, want zero score/reward and failed status", evaluation)
	}
	if chat.calls != 0 {
		t.Fatalf("judge called %d times, want 0 when no answer was produced", chat.calls)
	}
	reward, err := os.ReadFile(filepath.Join(benchmark.outputDir, task.ID, "evaluation", "reward.txt"))
	if err != nil || strings.TrimSpace(string(reward)) != "0" {
		t.Fatalf("reward.txt = %q, err = %v", reward, err)
	}
	if _, err := os.Stat(filepath.Join(benchmark.outputDir, task.ID, "evaluation", "evaluation_results.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evaluation_results.json exists (or errored unexpectedly) for a missing answer: %v", err)
	}
}

func TestEvaluateEmptyAnswerScoresZeroWithoutError(t *testing.T) {
	for name, content := range map[string]string{
		"blank":            "   \n",
		"tag with nothing": finalAnswerTag,
	} {
		t.Run(name, func(t *testing.T) {
			chat := &stubChat{}
			benchmark, task := benchmarkWithFixtureAndChat(t, chat)
			sandbox := &evaluateFake{downloadContent: content}

			evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if evaluation.Reward != 0 || evaluation.Score != 0 {
				t.Fatalf("evaluation = %#v, want zero score/reward", evaluation)
			}
			if chat.calls != 0 {
				t.Fatalf("judge called %d times, want 0 for an empty answer", chat.calls)
			}
		})
	}
}

func TestEvaluateExtractsFinalAnswerTag(t *testing.T) {
	chat := &stubChat{responses: []string{ratingResponse("YES"), ratingResponse("YES")}}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadContent: "reasoning that should be discarded\n" + finalAnswerTag + "\nthe real answer\n"}

	if _, err := benchmark.Evaluate(context.Background(), task, sandbox); err != nil {
		t.Fatal(err)
	}
	for _, prompt := range chat.prompts {
		if strings.Contains(prompt, "reasoning that should be discarded") {
			t.Fatalf("judge prompt leaked pre-tag content: %q", prompt)
		}
		if !strings.Contains(prompt, "the real answer") {
			t.Fatalf("judge prompt missing extracted answer: %q", prompt)
		}
	}
}

func TestEvaluateAllMustHavesPassSucceeds(t *testing.T) {
	// r1 is "must have" and scores YES; r2 is "nice to have" and scores NO —
	// reward only depends on must-have rubrics, but agg_score averages both.
	chat := &stubChat{responses: []string{ratingResponse("YES"), ratingResponse("NO")}}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadContent: finalAnswerTag + "\nthe answer\n"}

	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Status != core.StatusSucceeded || evaluation.VerifierStatus != core.StatusSucceeded {
		t.Fatalf("evaluation = %#v, want succeeded", evaluation)
	}
	if evaluation.Reward != 1 {
		t.Fatalf("Reward = %v, want 1", evaluation.Reward)
	}
	if evaluation.Score != 0.5 {
		t.Fatalf("Score = %v, want 0.5 (mean of YES=1 and NO=0)", evaluation.Score)
	}
}

func TestEvaluateMustHaveFailureFailsRegardlessOfOthers(t *testing.T) {
	chat := &stubChat{responses: []string{ratingResponse("NO"), ratingResponse("YES")}}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadContent: finalAnswerTag + "\nthe answer\n"}

	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Status == core.StatusSucceeded || evaluation.Reward != 0 {
		t.Fatalf("evaluation = %#v, want failed (a must-have rubric scored NO)", evaluation)
	}
}

func TestEvaluateRetriesJudgeErrorsThenSucceeds(t *testing.T) {
	restore := rubricRetryCapForTest(t)
	defer restore()
	chat := &stubChat{
		errs:      []error{errors.New("transient"), nil},
		responses: []string{"", ratingResponse("YES"), ratingResponse("YES")},
	}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadContent: finalAnswerTag + "\nthe answer\n"}

	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Reward != 1 {
		t.Fatalf("Reward = %v, want 1 after a transient judge error retried successfully", evaluation.Reward)
	}
	if chat.calls != 3 {
		t.Fatalf("chat.calls = %d, want 3 (1 failed + 1 retry for r1, 1 for r2)", chat.calls)
	}
}

func TestEvaluateUnscoredRubricAfterRetryExhaustionIsExcluded(t *testing.T) {
	responses := make([]string, maxRubricRetries)
	for i := range responses {
		responses[i] = "not json at all"
	}
	responses = append(responses, ratingResponse("YES"))
	chat := &stubChat{responses: responses}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadContent: finalAnswerTag + "\nthe answer\n"}

	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	// r1 (must have) never scored -> no scored must-haves -> reward 0.
	if evaluation.Reward != 0 {
		t.Fatalf("Reward = %v, want 0 when the only must-have rubric never scored", evaluation.Reward)
	}
	// agg_score is the mean over scored rubrics only: r2 alone, scored YES=1.
	if evaluation.Score != 1 {
		t.Fatalf("Score = %v, want 1 (mean over the one scored rubric)", evaluation.Score)
	}
	if chat.calls != maxRubricRetries+1 {
		t.Fatalf("chat.calls = %d, want %d", chat.calls, maxRubricRetries+1)
	}
}

// TestEvaluateWritesJudgeErrorsLogWhenJudgeCallsConsistentlyFail locks in the
// fix for a systemic judge failure (e.g. every call returning HTTP 401)
// silently looking identical to a legitimately-scored 0: unscored rubrics
// must leave a diagnosable trail, not just null scores.
func TestEvaluateWritesJudgeErrorsLogWhenJudgeCallsConsistentlyFail(t *testing.T) {
	restore := rubricRetryCapForTest(t)
	defer restore()
	errs := make([]error, maxRubricRetries*2)
	for i := range errs {
		errs[i] = errors.New("401 unauthorized")
	}
	chat := &stubChat{errs: errs}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadContent: finalAnswerTag + "\nthe answer\n"}

	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Reward != 0 || evaluation.Score != 0 {
		t.Fatalf("evaluation = %#v, want zero reward/score when every judge call fails", evaluation)
	}
	if evaluation.Error == "" {
		t.Fatal("evaluation.Error is empty despite every judge call failing")
	}
	logPath := filepath.Join(benchmark.outputDir, task.ID, "evaluation", "judge_errors.log")
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("judge_errors.log missing: %v", err)
	}
	if !strings.Contains(string(content), "401 unauthorized") {
		t.Fatalf("judge_errors.log = %q, want it to contain the underlying error", content)
	}
	found := false
	for _, path := range evaluation.LogPaths {
		if path == logPath {
			found = true
		}
	}
	if !found {
		t.Fatalf("LogPaths = %v, want it to include judge_errors.log", evaluation.LogPaths)
	}
}

func TestEvaluateWritesEvaluationResultsArtifact(t *testing.T) {
	chat := &stubChat{responses: []string{ratingResponse("YES"), ratingResponse("YES")}}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadContent: finalAnswerTag + "\nthe answer\n"}

	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.LogPaths) != 3 {
		t.Fatalf("LogPaths = %v, want answer/reward/results", evaluation.LogPaths)
	}
	resultsBytes, err := os.ReadFile(filepath.Join(benchmark.outputDir, task.ID, "evaluation", "evaluation_results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded evaluationResults
	if err := json.Unmarshal(resultsBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Reward != 1 || decoded.NumRubrics != 2 || decoded.NumScored != 2 || decoded.NumPassed != 2 {
		t.Fatalf("decoded results = %#v", decoded)
	}
}

func TestEvaluateNeverUploadsToSandbox(t *testing.T) {
	chat := &stubChat{responses: []string{ratingResponse("YES"), ratingResponse("YES")}}
	benchmark, task := benchmarkWithFixtureAndChat(t, chat)
	sandbox := &evaluateFake{downloadContent: finalAnswerTag + "\nthe answer\n"}

	if _, err := benchmark.Evaluate(context.Background(), task, sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.uploads != 0 {
		t.Fatalf("Evaluate uploaded %d files; rubric/prompt material must stay host-side", sandbox.uploads)
	}
	if sandbox.downloads != 1 || sandbox.downloadSource != answerPath {
		t.Fatalf("downloads = %d, source = %q, want exactly one download from %q", sandbox.downloads, sandbox.downloadSource, answerPath)
	}
}

// rubricRetryCapForTest shrinks the retry backoff so retry tests don't wait
// out real wall-clock seconds, restoring it afterward.
func rubricRetryCapForTest(t *testing.T) func() {
	t.Helper()
	restoreDelay, restoreCap := rubricRetryBaseDelay, rubricRetryCap
	rubricRetryBaseDelay = time.Millisecond
	rubricRetryCap = 10 * time.Millisecond
	return func() { rubricRetryBaseDelay, rubricRetryCap = restoreDelay, restoreCap }
}
