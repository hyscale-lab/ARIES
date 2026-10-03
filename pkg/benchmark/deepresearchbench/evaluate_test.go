package deepresearchbench

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

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

// stubRace is a raceScorer fake driven by a fixed overall ratio (in [0,1]),
// exactly like upstream's target/(target+reference) normalization.
type stubRace struct {
	result raceResult
	err    error
	calls  int
	prompt string
	report string
	ref    string
}

func (r *stubRace) Score(_ context.Context, prompt, targetArticle, referenceArticle string, _ taskCriteria) (raceResult, error) {
	r.calls++
	r.prompt, r.report, r.ref = prompt, targetArticle, referenceArticle
	return r.result, r.err
}

// stubFact is a factRunner fake for exercising the FACT-enabled/disabled/
// error paths in Evaluate independently of the real HTTP-backed pipeline.
type stubFact struct {
	report factReport
	err    error
	calls  int
}

func (f *stubFact) Run(context.Context, string) (factReport, error) {
	f.calls++
	return f.report, f.err
}

func newTestBenchmark(t *testing.T, race raceScorer) (*Benchmark, string) {
	t.Helper()
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.OutputDir = t.TempDir()
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if race != nil {
		benchmark.race = race
	}
	if _, err := benchmark.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	return benchmark, root
}

func newJudgeDisabledTestBenchmark(t *testing.T) *Benchmark {
	t.Helper()
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.Judge = core.ModelConfig{}
	options.JudgeDisabled = true
	options.OutputDir = t.TempDir()
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	return benchmark
}

// When the judge is disabled, Evaluate must still download and retain the
// report, but skip RACE entirely: no judge_prompt.txt/judge_response.json,
// and Status/VerifierStatus report core.StatusNotEnabled (not succeeded or
// failed) with Score/Reward left at zero.
func TestEvaluateReportsNotEnabledWhenJudgeDisabled(t *testing.T) {
	benchmark := newJudgeDisabledTestBenchmark(t)
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Status != core.StatusNotEnabled || evaluation.VerifierStatus != core.StatusNotEnabled {
		t.Fatalf("evaluation status = %+v, want not_enabled", evaluation)
	}
	if evaluation.Score != 0 || evaluation.Reward != 0 {
		t.Fatalf("evaluation = %+v, want Score=0 Reward=0", evaluation)
	}
	if len(evaluation.LogPaths) != 1 {
		t.Fatalf("LogPaths = %v, want only the report artifact when judging is disabled", evaluation.LogPaths)
	}
	if _, err := os.Stat(evaluation.LogPaths[0]); err != nil {
		t.Fatalf("report artifact missing: %v", err)
	}
	artifactDir := filepath.Dir(evaluation.LogPaths[0])
	if _, err := os.Stat(filepath.Join(artifactDir, "judge_prompt.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("judge_prompt.txt exists (or errored unexpectedly) despite judging being disabled: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artifactDir, "judge_response.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("judge_response.json exists (or errored unexpectedly) despite judging being disabled: %v", err)
	}
}

func TestEvaluateRequiresLiveSandbox(t *testing.T) {
	benchmark, _ := newTestBenchmark(t, nil)
	if _, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, nil); err == nil {
		t.Fatal("Evaluate accepted a nil sandbox")
	}
}

func TestEvaluateMissingReportScoresZeroWithoutError(t *testing.T) {
	race := &stubRace{}
	benchmark, _ := newTestBenchmark(t, race)
	sandbox := &evaluateFake{downloadErr: errors.New("no such file")}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatalf("Evaluate returned an error for a missing report: %v", err)
	}
	if evaluation.Score != 0 || evaluation.Reward != 0 {
		t.Fatalf("evaluation = %+v, want zero score/reward", evaluation)
	}
	if evaluation.Error == "" {
		t.Fatal("evaluation.Error is empty for a missing report")
	}
	if race.calls != 0 {
		t.Fatalf("race judge called %d times, want 0 when no report was produced", race.calls)
	}
}

func TestEvaluateNeverUploadsReferenceArticle(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.8}}
	benchmark, _ := newTestBenchmark(t, race)
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	if _, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.uploads != 0 {
		t.Fatalf("Evaluate uploaded %d files into the sandbox; reference material must stay host-side", sandbox.uploads)
	}
	if race.ref != "reference report" {
		t.Fatalf("race judge received reference = %q, want the fixture's reference article", race.ref)
	}
}

func TestEvaluateDownloadsFromFixedReportPath(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.8}}
	benchmark, _ := newTestBenchmark(t, race)
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	if _, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.downloads != 1 || sandbox.downloadSource != reportPath {
		t.Fatalf("downloadSource = %q, downloads = %d, want %q once", sandbox.downloadSource, sandbox.downloads, reportPath)
	}
	if race.report != "candidate report" {
		t.Fatalf("race judge received report = %q, want downloaded content", race.report)
	}
}

func TestEvaluateAboveThresholdSucceeds(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.75}}
	benchmark, _ := newTestBenchmark(t, race)
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Score != 75 || evaluation.Reward != 1 {
		t.Fatalf("evaluation = %+v, want Score=75 Reward=1", evaluation)
	}
	if evaluation.Status != core.StatusSucceeded || evaluation.VerifierStatus != core.StatusSucceeded {
		t.Fatalf("evaluation status = %+v, want succeeded", evaluation)
	}
}

func TestEvaluateBelowThresholdFails(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.40}}
	benchmark, _ := newTestBenchmark(t, race)
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Score != 40 || evaluation.Reward != 0 {
		t.Fatalf("evaluation = %+v, want Score=40 Reward=0", evaluation)
	}
	if evaluation.Status == core.StatusSucceeded {
		t.Fatalf("evaluation status = %+v, want not succeeded", evaluation)
	}
}

func TestEvaluateAtExactThresholdSucceeds(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: defaultRewardThreshold / 100.0}}
	benchmark, _ := newTestBenchmark(t, race)
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Reward != 1 {
		t.Fatalf("evaluation.Reward = %v, want 1 at the exact threshold", evaluation.Reward)
	}
}

func TestEvaluateWritesFullRaceDimensionBreakdownArtifact(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.8, Comprehensiveness: 0.7, Insight: 0.6, InstructionFollowing: 0.9, Readability: 0.5}}
	benchmark, _ := newTestBenchmark(t, race)
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	// The RACE sub-dimension breakdown lives only in the judge_response.json
	// artifact (core.Evaluation carries just the single Score/Reward pair).
	responseArtifact, err := os.ReadFile(evaluation.LogPaths[2])
	if err != nil {
		t.Fatal(err)
	}
	var decoded raceArtifact
	if err := json.Unmarshal(responseArtifact, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Overall != 0.8 || decoded.Comprehensiveness != 0.7 || decoded.Insight != 0.6 ||
		decoded.InstructionFollowing != 0.9 || decoded.Readability != 0.5 {
		t.Fatalf("decoded race artifact = %+v, want the full dimension breakdown", decoded)
	}
	if len(evaluation.LogPaths) != 3 {
		t.Fatalf("LogPaths = %v, want no fact artifacts when FACT is not configured", evaluation.LogPaths)
	}
}

func TestEvaluateWritesArtifacts(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.8}}
	benchmark, root := newTestBenchmark(t, race)
	_ = root
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(evaluation.LogPaths) != 3 {
		t.Fatalf("LogPaths = %v", evaluation.LogPaths)
	}
	reportArtifact, err := os.ReadFile(evaluation.LogPaths[0])
	if err != nil || string(reportArtifact) != "candidate report" {
		t.Fatalf("report artifact = %q, err = %v", reportArtifact, err)
	}
	promptArtifact, err := os.ReadFile(evaluation.LogPaths[1])
	if err != nil || string(promptArtifact) != "research prompt 1" {
		t.Fatalf("prompt artifact = %q, err = %v", promptArtifact, err)
	}
	responseArtifact, err := os.ReadFile(evaluation.LogPaths[2])
	if err != nil {
		t.Fatal(err)
	}
	var decoded raceArtifact
	if err := json.Unmarshal(responseArtifact, &decoded); err != nil || decoded.Overall != 0.8 {
		t.Fatalf("response artifact = %q, err = %v", responseArtifact, err)
	}
	for _, path := range evaluation.LogPaths {
		if filepath.Dir(path) != filepath.Join(benchmark.outputDir, "1", "evaluation") {
			t.Fatalf("artifact %q not under expected evaluation directory", path)
		}
	}
}

func TestEvaluateFailsClosedOnRaceError(t *testing.T) {
	race := &stubRace{err: errors.New("race judge unavailable")}
	benchmark, _ := newTestBenchmark(t, race)
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	if _, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox); err == nil {
		t.Fatal("Evaluate accepted a failing race judge call")
	}
}

func TestEvaluateWithFactEnabledWritesFactArtifact(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.8}}
	benchmark, _ := newTestBenchmark(t, race)
	fact := &stubFact{report: factReport{HasCitations: true, TotalCitations: 4, EffectiveCitations: 3}}
	benchmark.fact = fact
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if fact.calls != 1 {
		t.Fatalf("fact.calls = %d, want 1", fact.calls)
	}
	if len(evaluation.LogPaths) != 4 {
		t.Fatalf("LogPaths = %v, want a 4th path for fact_report.json", evaluation.LogPaths)
	}
	factArtifact, err := os.ReadFile(evaluation.LogPaths[3])
	if err != nil {
		t.Fatal(err)
	}
	var decoded factReport
	if err := json.Unmarshal(factArtifact, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TotalCitations != 4 || decoded.EffectiveCitations != 3 {
		t.Fatalf("decoded fact artifact = %+v, want TotalCitations=4 EffectiveCitations=3", decoded)
	}
}

func TestEvaluateWithFactZeroCitationsStillWritesArtifact(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.8}}
	benchmark, _ := newTestBenchmark(t, race)
	benchmark.fact = &stubFact{report: factReport{HasCitations: false}}
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	factArtifact, err := os.ReadFile(evaluation.LogPaths[3])
	if err != nil {
		t.Fatal(err)
	}
	var decoded factReport
	if err := json.Unmarshal(factArtifact, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.HasCitations || decoded.TotalCitations != 0 || decoded.EffectiveCitations != 0 {
		t.Fatalf("decoded fact artifact = %+v, want the zero-citations report", decoded)
	}
}

func TestEvaluateFactErrorDoesNotFailRaceDrivenOutcome(t *testing.T) {
	race := &stubRace{result: raceResult{Overall: 0.8}}
	benchmark, _ := newTestBenchmark(t, race)
	benchmark.fact = &stubFact{err: errors.New("fact pipeline unavailable")}
	sandbox := &evaluateFake{downloadContent: "candidate report"}
	evaluation, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, sandbox)
	if err != nil {
		t.Fatalf("Evaluate returned an error when only FACT failed: %v", err)
	}
	if evaluation.Score != 80 || evaluation.Reward != 1 || evaluation.Status != core.StatusSucceeded {
		t.Fatalf("evaluation = %+v, want RACE-driven success despite FACT failure", evaluation)
	}
	if len(evaluation.LogPaths) != 4 {
		t.Fatalf("LogPaths = %v, want a 4th path for fact_error.txt", evaluation.LogPaths)
	}
	factErrorArtifact, err := os.ReadFile(evaluation.LogPaths[3])
	if err != nil || string(factErrorArtifact) != "fact pipeline unavailable" {
		t.Fatalf("fact error artifact = %q, err = %v", factErrorArtifact, err)
	}
}

func (s *evaluateFake) NetworkName() string { return "test-network" }
