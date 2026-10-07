//go:build integration

package deepresearchbench

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

func TestPinnedDatasetSetupIsIdempotent(t *testing.T) {
	root, versions := requirePinnedDataset(t)
	if err := Setup(context.Background(), root, versions.DeepResearchBench.RepositoryURL, versions.DeepResearchBench.Revision); err != nil {
		t.Fatalf("idempotent Setup() error = %v", err)
	}
}

func TestPinnedAllTasksLoad(t *testing.T) {
	root, versions := requirePinnedDataset(t)
	taskIDs := make([]string, expectedTaskCount)
	for id := 1; id <= expectedTaskCount; id++ {
		taskIDs[id-1] = strconv.Itoa(id)
	}
	benchmark, err := New(Options{
		Root: root, TaskIDs: taskIDs, OutputDir: t.TempDir(),
		Revision:     versions.DeepResearchBench.Revision,
		Environment:  core.Environment{Image: "aries/deep-research-bench:latest", Workdir: "/workspace"},
		Judge:        core.ModelConfig{Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4.1", APIKeyEnv: "OPENAI_API_KEY"},
		APIKeyLookup: fakeAPIKeyLookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != expectedTaskCount {
		t.Fatalf("len(tasks) = %d, want %d", len(tasks), expectedTaskCount)
	}
	seen := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		if _, duplicate := seen[task.ID]; duplicate {
			t.Fatalf("duplicate task ID %q", task.ID)
		}
		seen[task.ID] = struct{}{}
		if strings.TrimSpace(task.Instruction) == "" {
			t.Fatalf("task %q has an empty instruction", task.ID)
		}
		if !task.Environment.AllowNetwork {
			t.Fatalf("task %q Environment.AllowNetwork = false, want true", task.ID)
		}
	}
}

func TestPinnedSpecificTaskPromptMatches(t *testing.T) {
	root, versions := requirePinnedDataset(t)
	want := map[string]string{
		"51": "elderly people",
	}
	taskIDs := make([]string, 0, len(want))
	for id := range want {
		taskIDs = append(taskIDs, id)
	}
	benchmark, err := New(Options{
		Root: root, TaskIDs: taskIDs, OutputDir: t.TempDir(),
		Revision:     versions.DeepResearchBench.Revision,
		Environment:  core.Environment{Image: "aries/deep-research-bench:latest", Workdir: "/workspace"},
		Judge:        core.ModelConfig{Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4.1", APIKeyEnv: "OPENAI_API_KEY"},
		APIKeyLookup: fakeAPIKeyLookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		substring, ok := want[task.ID]
		if !ok {
			t.Fatalf("unexpected task %q", task.ID)
		}
		if !strings.Contains(task.Instruction, substring) {
			t.Fatalf("task %q instruction = %q, want substring %q", task.ID, task.Instruction, substring)
		}
	}
}

// TestPinnedPrepareSandboxAndEvaluateAgainstFakeSandbox exercises
// PrepareSandbox and Evaluate against the real pinned checkout's prompts and
// reference articles, using an in-memory fake sandbox and a stubbed judge so
// the test needs neither Docker nor a live judge API key. The real judge HTTP
// call is covered separately by judge_test.go's httptest-server tests.
func TestPinnedPrepareSandboxAndEvaluateAgainstFakeSandbox(t *testing.T) {
	root, versions := requirePinnedDataset(t)
	benchmark, err := New(Options{
		Root: root, TaskIDs: []string{"1"}, OutputDir: t.TempDir(),
		Revision:     versions.DeepResearchBench.Revision,
		Environment:  core.Environment{Image: "aries/deep-research-bench:latest", Workdir: "/workspace"},
		Judge:        core.ModelConfig{Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "gpt-4.1", APIKeyEnv: "OPENAI_API_KEY"},
		APIKeyLookup: fakeAPIKeyLookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	benchmark.race = &stubRace{result: raceResult{Overall: 0.9}}

	tasks, err := benchmark.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v", tasks)
	}
	task := tasks[0]

	sandbox := &evaluateFake{downloadContent: "a synthetic candidate report"}
	if err := benchmark.PrepareSandbox(context.Background(), task, sandbox); err != nil {
		t.Fatalf("PrepareSandbox() error = %v", err)
	}
	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox, nil)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if evaluation.Score != 90 || evaluation.Reward != 1 || evaluation.Status != core.StatusSucceeded {
		t.Fatalf("evaluation = %+v", evaluation)
	}
	if sandbox.downloadSource != reportPath {
		t.Fatalf("downloadSource = %q, want %q", sandbox.downloadSource, reportPath)
	}
}

func requirePinnedDataset(t *testing.T) (string, config.Versions) {
	t.Helper()
	versions, err := config.LoadVersions(filepath.Clean(filepath.Join("..", "..", "..", "configs", "versions.json")))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join("..", "..", "..", DefaultRoot))
	if err := Setup(context.Background(), root, versions.DeepResearchBench.RepositoryURL, versions.DeepResearchBench.Revision); err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if err := VerifyRevision(context.Background(), root, versions.DeepResearchBench.Revision); err != nil {
		t.Fatal(err)
	}
	return root, versions
}
