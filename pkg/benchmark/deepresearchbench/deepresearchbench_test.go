package deepresearchbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

// fixtureRow is the shape shared by the three vendored fixture files: full
// query rows (id/prompt used by loadPrompts) plus a reference article
// (used by loadReferenceArticles). Criteria fixtures are written separately
// via defaultCriteriaFixtureRows/writeJSONLFile since their shape doesn't
// overlap.
type fixtureRow struct {
	ID      int    `json:"id"`
	Prompt  string `json:"prompt"`
	Article string `json:"article"`
}

func defaultFixtureRows(t *testing.T) []fixtureRow {
	t.Helper()
	rows := make([]fixtureRow, 0, expectedTaskCount)
	for id := 1; id <= expectedTaskCount; id++ {
		rows = append(rows, fixtureRow{ID: id, Prompt: fmt.Sprintf("research prompt %d", id), Article: "reference report"})
	}
	return rows
}

func defaultCriteriaFixtureRows(t *testing.T) []criteriaRow {
	t.Helper()
	rows := make([]criteriaRow, 0, expectedTaskCount)
	oneCriterion := []criterion{{Criterion: "coverage", Explanation: "breadth", Weight: 1.0}}
	for id := 1; id <= expectedTaskCount; id++ {
		rows = append(rows, criteriaRow{
			ID:     id,
			Prompt: fmt.Sprintf("research prompt %d", id),
			DimensionWeight: dimensionWeights{
				Comprehensiveness: 0.25, Insight: 0.25, InstructionFollowing: 0.25, Readability: 0.25,
			},
			Criterions: dimensionCriteria{
				Comprehensiveness:    oneCriterion,
				Insight:              oneCriterion,
				InstructionFollowing: oneCriterion,
				Readability:          oneCriterion,
			},
		})
	}
	return rows
}

// writeJSONLFile writes rows (any JSON-marshalable slice) as newline-
// delimited JSON at relPath under root, creating parent directories as
// needed.
func writeJSONLFile[T any](t *testing.T, root, relPath string, rows []T) {
	t.Helper()
	var builder strings.Builder
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		builder.Write(encoded)
		builder.WriteByte('\n')
	}
	path := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeFixture writes rows as both the query file and the reference file
// (fixtureRow carries both a prompt and an article), plus a matching
// criteria file, and commits the result as a fresh git repository so
// VerifyRevision can pin against it.
func writeFixture(t *testing.T, rows []fixtureRow) string {
	t.Helper()
	root := t.TempDir()
	writeJSONLFile(t, root, DefaultQueryFile, rows)
	writeJSONLFile(t, root, DefaultReferenceFile, rows)
	writeJSONLFile(t, root, DefaultCriteriaFile, defaultCriteriaFixtureRows(t))
	commitFixture(t, root)
	return root
}

func commitFixture(t *testing.T, root string) {
	t.Helper()
	commands := [][]string{
		{"init", "--quiet"},
		{"add", "."},
		{"-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "commit", "--quiet", "-m", "fixture"},
	}
	for _, arguments := range commands {
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
}

func fixtureGitRevision(root string) string {
	output, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return strings.Repeat("0", 40)
	}
	return strings.TrimSpace(string(output))
}

func fakeAPIKeyLookup(string) ([]byte, bool) { return []byte("fake-key"), true }

func baseOptions(root string) Options {
	return Options{
		Root:      root,
		TaskIDs:   []string{"1"},
		OutputDir: "out",
		Revision:  fixtureGitRevision(root),
		Environment: core.Environment{
			Image:   "aries/deep-research-bench:latest",
			Workdir: "/workspace",
		},
		Judge: core.ModelConfig{
			Provider:  "openai",
			BaseURL:   "https://api.openai.com/v1",
			Model:     "gpt-4.1",
			APIKeyEnv: "OPENAI_API_KEY",
		},
		APIKeyLookup: fakeAPIKeyLookup,
	}
}

func TestTasksLoadsSelectedPromptsInOrder(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.TaskIDs = []string{"3", "1", "100"}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Fatalf("tasks = %+v", tasks)
	}
	wantIDs := []string{"3", "1", "100"}
	wantPrompts := []string{
		applyPromptTemplate(taskPromptTemplate, "research prompt 3") + reportInstruction,
		applyPromptTemplate(taskPromptTemplate, "research prompt 1") + reportInstruction,
		applyPromptTemplate(taskPromptTemplate, "research prompt 100") + reportInstruction,
	}
	for index, task := range tasks {
		if task.ID != wantIDs[index] {
			t.Fatalf("tasks[%d].ID = %q, want %q", index, task.ID, wantIDs[index])
		}
		if task.Instruction != wantPrompts[index] {
			t.Fatalf("tasks[%d].Instruction = %q, want %q", index, task.Instruction, wantPrompts[index])
		}
		if task.Environment.Services.SearchPort != 8888 {
			t.Fatalf("search declaration = %#v", task.Environment.Services)
		}
		if !task.Environment.AllowNetwork {
			t.Fatalf("tasks[%d].Environment.AllowNetwork = false, want true", index)
		}
	}
}

func TestTasksAppliesTaskPromptTemplate(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := applyPromptTemplate(taskPromptTemplate, "research prompt 1") + reportInstruction
	if tasks[0].Instruction != want {
		t.Fatalf("Instruction = %q, want %q", tasks[0].Instruction, want)
	}
	if !strings.Contains(tasks[0].Instruction, "research prompt 1") {
		t.Fatalf("Instruction = %q, want it to contain the task's research prompt", tasks[0].Instruction)
	}
}

func TestTasksRejectsRevisionMismatch(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.Revision = strings.Repeat("f", 40)
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err == nil {
		t.Fatal("Tasks accepted a mismatched revision")
	}
}

func TestTasksRejectsWrongRowCount(t *testing.T) {
	rows := defaultFixtureRows(t)[:expectedTaskCount-1]
	root := writeFixture(t, rows)
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err == nil || !strings.Contains(err.Error(), "expected exactly") {
		t.Fatalf("Tasks error = %v, want row-count mismatch", err)
	}
}

func TestTasksRejectsDuplicateIDs(t *testing.T) {
	rows := defaultFixtureRows(t)
	rows[1] = rows[0]
	root := writeFixture(t, rows)
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("Tasks error = %v, want duplicate id rejection", err)
	}
}

func TestTasksRejectsEmptyPrompt(t *testing.T) {
	rows := defaultFixtureRows(t)
	rows[0].Prompt = "   "
	root := writeFixture(t, rows)
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Tasks(context.Background()); err == nil || !strings.Contains(err.Error(), "empty prompt") {
		t.Fatalf("Tasks error = %v, want empty prompt rejection", err)
	}
}

func TestNewRejectsInvalidTaskIDs(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	for _, id := range []string{"0", "101", "-1", "01", "abc", "", " 1", "1 "} {
		options := baseOptions(root)
		options.TaskIDs = []string{id}
		if _, err := New(options); err == nil {
			t.Fatalf("accepted invalid task ID %q", id)
		}
	}
}

func TestNewRejectsDuplicateTaskIDs(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.TaskIDs = []string{"1", "1"}
	if _, err := New(options); err == nil {
		t.Fatal("accepted duplicate task IDs")
	}
}

func TestNewRequiresEnvironmentImage(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.Environment.Image = ""
	if _, err := New(options); err == nil {
		t.Fatal("accepted an empty environment image")
	}
}

func TestPrepareSandboxAndEvaluateRequireLiveSandbox(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "1"}, nil); err == nil {
		t.Fatal("PrepareSandbox accepted a nil sandbox")
	}
	if _, err := benchmark.Evaluate(context.Background(), core.Task{ID: "1"}, nil); err == nil {
		t.Fatal("Evaluate accepted a nil sandbox")
	}
}

func TestNewRequiresJudgeConfig(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.Judge = core.ModelConfig{}
	if _, err := New(options); err == nil {
		t.Fatal("accepted an empty judge model config")
	}
}

func TestNewAllowsEmptyJudgeWhenJudgeDisabled(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.Judge = core.ModelConfig{}
	options.JudgeDisabled = true
	benchmark, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v, want an empty judge model config accepted when JudgeDisabled is true", err)
	}
	if benchmark.race != nil {
		t.Fatal("race is non-nil, want RACE disabled when JudgeDisabled is true")
	}
}

func TestNewRejectsJudgeConfigWhenJudgeDisabled(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.JudgeDisabled = true
	if _, err := New(options); err == nil {
		t.Fatal("accepted a non-empty judge model config alongside JudgeDisabled")
	}
}

func TestNewSkipsFactWithReasonWhenJudgeDisabled(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := factOptions(root)
	options.Judge = core.ModelConfig{}
	options.JudgeDisabled = true
	benchmark, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v, want a configured fact block accepted (and ignored) when JudgeDisabled is true", err)
	}
	if benchmark.race != nil {
		t.Fatal("race is non-nil, want RACE disabled when JudgeDisabled is true")
	}
	if benchmark.fact != nil {
		t.Fatal("fact is non-nil, want FACT disabled when JudgeDisabled is true even though FactJudge/JinaAPIKeyEnv were set")
	}
	if benchmark.FactSkipReason() == "" {
		t.Fatal("FactSkipReason() is empty, want an explanation that the judge being disabled also skipped FACT")
	}
}

func TestNewRequiresAPIKeyLookup(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.APIKeyLookup = nil
	if _, err := New(options); err == nil {
		t.Fatal("accepted a nil API key lookup")
	}
}

func TestNewRejectsMissingAPIKey(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.APIKeyLookup = func(string) ([]byte, bool) { return nil, false }
	if _, err := New(options); err == nil {
		t.Fatal("accepted a missing judge API key")
	}
}

func TestNewRejectsInvalidRewardThreshold(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	for _, threshold := range []float64{-1, 101, -100} {
		options := baseOptions(root)
		options.RewardThreshold = threshold
		if _, err := New(options); err == nil {
			t.Fatalf("accepted invalid reward threshold %v", threshold)
		}
	}
}

func TestTasksRemapsToExecutionTaskID(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := baseOptions(root)
	options.ExecutionTaskIDs = []string{"1-001"}
	benchmark, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "1-001" {
		t.Fatalf("tasks = %+v, want ID 1-001", tasks)
	}
	benchmark.mu.RLock()
	numericID, ok := benchmark.numericIDs["1-001"]
	benchmark.mu.RUnlock()
	if !ok || numericID != 1 {
		t.Fatalf("numericIDs[1-001] = %d, ok = %v, want 1, true", numericID, ok)
	}
}

func TestNewRejectsMismatchedExecutionTaskIDs(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	for _, executionIDs := range [][]string{
		{"1-001", "2-001"},
		{"2-001"},
		{"1-000"},
		{"1"},
	} {
		options := baseOptions(root)
		options.ExecutionTaskIDs = executionIDs
		if _, err := New(options); err == nil {
			t.Fatalf("accepted invalid execution task IDs %v", executionIDs)
		}
	}
}

func TestNewDefaultsRewardThreshold(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if benchmark.rewardThreshold != defaultRewardThreshold {
		t.Fatalf("rewardThreshold = %v, want %v", benchmark.rewardThreshold, defaultRewardThreshold)
	}
}

func factOptions(root string) Options {
	options := baseOptions(root)
	options.FactJudge = core.ModelConfig{
		Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4.1-mini", APIKeyEnv: "OPENAI_API_KEY",
	}
	options.JinaAPIKeyEnv = "JINA_API_KEY"
	return options
}

func TestNewEnablesFactWhenAllKeysArePresent(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(factOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if benchmark.fact == nil {
		t.Fatal("fact is nil, want FACT enabled when the judge and Jina keys both resolve")
	}
	if benchmark.FactSkipReason() != "" {
		t.Fatalf("FactSkipReason() = %q, want empty when FACT is enabled", benchmark.FactSkipReason())
	}
}

func TestNewSkipsFactWhenJinaKeyIsNotSet(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := factOptions(root)
	options.APIKeyLookup = func(name string) ([]byte, bool) {
		if name == options.JinaAPIKeyEnv {
			return nil, false
		}
		return []byte("fake-key"), true
	}
	benchmark, err := New(options)
	if err != nil {
		t.Fatalf("New() error = %v, want a missing Jina key to skip FACT rather than fail construction", err)
	}
	if benchmark.fact != nil {
		t.Fatal("fact is non-nil, want FACT disabled when the Jina key is not set")
	}
	if benchmark.FactSkipReason() == "" {
		t.Fatal("FactSkipReason() is empty, want an explanation for why FACT was skipped")
	}
}

func TestFactSkipReasonEmptyWhenFactNeverConfigured(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	benchmark, err := New(baseOptions(root))
	if err != nil {
		t.Fatal(err)
	}
	if benchmark.fact != nil {
		t.Fatal("fact is non-nil, want FACT disabled when no fact options were supplied at all")
	}
	if benchmark.FactSkipReason() != "" {
		t.Fatalf("FactSkipReason() = %q, want empty when FACT was never configured", benchmark.FactSkipReason())
	}
}

func TestNewStillRejectsIncompleteFactModelConfig(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := factOptions(root)
	options.FactJudge.Model = ""
	if _, err := New(options); err == nil {
		t.Fatal("accepted an incomplete FACT judge model config")
	}
}

func TestNewStillRejectsEmptyJinaAPIKeyEnvName(t *testing.T) {
	root := writeFixture(t, defaultFixtureRows(t))
	options := factOptions(root)
	options.JinaAPIKeyEnv = ""
	if _, err := New(options); err == nil {
		t.Fatal("accepted a FACT config with no Jina API key environment variable name")
	}
}
