package sweatlas

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	qaTaskID        = "task-6905333b74f22949d97ba998"
	qaTaskWorkdir   = "/"
	qaTaskImage     = "example.invalid/swe-atlas-qa:fixture"
	qaInstruction   = "I am onboarding on a codebase. Write your answer to /logs/agent/answer.txt.\n"
	fixtureRevision = "cccccccccccccccccccccccccccccccccccccccc"
)

const fixtureTaskTOML = `schema_version = "1.1"

[task]
name = "scale-ai/task-6905333b74f22949d97ba998"
description = ""
authors = []
keywords = []

[metadata]
category = "Code Onboarding"
language = "ts"
repository = "Automattic/wp-calypso"
base_commit = "be7e5cc641622d153040491fd5625c6cb83e12eb"

[verifier]
timeout_sec = 900.0

[agent]
timeout_sec = 10800.0

[environment]
build_timeout_sec = 600.0
docker_image = "example.invalid/swe-atlas-qa:fixture"
cpus = 16
memory_mb = 16384
storage_mb = 20480
gpus = 0
allow_internet = true
mcp_servers = []

[verifier.env]
EVAL_API_KEY = "${OPENAI_API_KEY}"
EVAL_BASE_URL = "${OPENAI_API_BASE}"
EVAL_MODEL = "${EVAL_MODEL:-anthropic/claude-opus-4-5-20251101}"

[environment.env]

[solution.env]
`

// r1 is explicitly "must have"; r2 is explicitly "nice to have" so tests can
// exercise reward's must-have-only gating distinctly from agg_score's
// mean-over-all-scored math. The real vendored dataset never actually sets a
// top-level "importance" field (see rubric's doc comment), but the struct
// supports it and this fixture exercises that code path directly.
const fixtureRubrics = `[
  {"id": "r1", "title": "1.1: States the port number", "importance": "must have", "annotations": {"type": "positive"}},
  {"id": "r2", "title": "1.2: States a secondary detail", "importance": "nice to have", "annotations": {"type": "positive"}}
]`

const fixtureSystemPrompt = "You are a strict grader.\n"

const fixtureUserPromptTemplate = "# Prompt\n{problem_statement}\n\n# Response\n{model_answer}\n\n#Rubric Criteria\n{{\n  \"rubric_statement\": {title}\n}}"

func writeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	taskDir := filepath.Join(root, qaSubdirectory, qaTaskID)
	writeFile(t, filepath.Join(taskDir, "task.toml"), fixtureTaskTOML)
	writeFile(t, filepath.Join(taskDir, "instruction.md"), qaInstruction)
	writeFile(t, filepath.Join(taskDir, "environment", "Dockerfile"), "FROM python:3.13-slim-bookworm\n")
	writeFile(t, filepath.Join(taskDir, "tests", "rubrics.json"), fixtureRubrics)
	writeFile(t, filepath.Join(taskDir, "tests", "system_prompt.txt"), fixtureSystemPrompt)
	writeFile(t, filepath.Join(taskDir, "tests", "user_prompt_template.txt"), fixtureUserPromptTemplate)
	writeFile(t, filepath.Join(taskDir, "tests", "prompt.txt"), "What port does the dev server use?\n")
	writeFile(t, filepath.Join(taskDir, "solution", "answer.txt"), "reference answer\n")
	commitFixture(t, root)
	return root
}

func testOptions(root string, taskIDs []string, outputDir string) Options {
	return Options{
		Root: root, TaskIDs: taskIDs, OutputDir: outputDir,
		Revision: fixtureGitRevision(root),
		Judge:    core.ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY"},
		APIKeyLookup: func(name string) ([]byte, bool) {
			if name == "DEEPSEEK_API_KEY" {
				return []byte("fake-key"), true
			}
			return nil, false
		},
	}
}

func TestLoadTaskMapsGenericFieldsAndKeepsVerifierPrivate(t *testing.T) {
	root := writeFixture(t)

	task, details, err := loadTask(root, qaTaskID)
	if err != nil {
		t.Fatalf("loadTask() error = %v", err)
	}
	if task.ID != qaTaskID || task.Instruction != strings.TrimSpace(qaInstruction) {
		t.Fatalf("task identity/instruction = %#v", task)
	}
	wantEnvironment := core.Environment{
		Image:        qaTaskImage,
		Workdir:      qaTaskWorkdir,
		CPU:          16,
		MemoryMB:     16384,
		GPUs:         0,
		AllowNetwork: true,
	}
	if !reflect.DeepEqual(task.Environment, wantEnvironment) {
		t.Fatalf("Environment = %#v, want %#v", task.Environment, wantEnvironment)
	}
	if task.Timeout != 3*time.Hour {
		t.Fatalf("agent timeout = %s, want 3h", task.Timeout)
	}
	if details.timeout != 15*time.Minute {
		t.Fatalf("verifier timeout = %s, want 15m", details.timeout)
	}
	if details.testsDir == "" {
		t.Fatal("testsDir is empty")
	}
	if _, err := os.Stat(filepath.Join(details.testsDir, "rubrics.json")); err != nil {
		t.Fatalf("testsDir does not contain rubrics.json: %v", err)
	}

	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"rubrics", "1.1: States", "reference answer"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("generic task JSON exposes private value %q: %s", private, encoded)
		}
	}
}

func TestLoadTaskRejectsWrongNamePrefix(t *testing.T) {
	root := writeFixture(t)
	path := filepath.Join(root, qaSubdirectory, qaTaskID, "task.toml")
	content := strings.Replace(fixtureTaskTOML, `name = "scale-ai/task-6905333b74f22949d97ba998"`, `name = "terminal-bench/task-6905333b74f22949d97ba998"`, 1)
	writeFile(t, path, content)

	if _, _, err := loadTask(root, qaTaskID); err == nil || !strings.Contains(err.Error(), "task identity") {
		t.Fatalf("loadTask() error = %v, want task identity mismatch", err)
	}
}

func TestLoadTaskRequiresRubricsAndPrompts(t *testing.T) {
	for _, name := range []string{"rubrics.json", "system_prompt.txt", "user_prompt_template.txt"} {
		t.Run(name, func(t *testing.T) {
			root := writeFixture(t)
			if err := os.Remove(filepath.Join(root, qaSubdirectory, qaTaskID, "tests", name)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := loadTask(root, qaTaskID); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("loadTask() error = %v, want missing %s", err, name)
			}
		})
	}
}

func TestNewRequiresJudgeAndAPIKeyLookup(t *testing.T) {
	base := testOptions("root", []string{qaTaskID}, "out")
	for name, mutate := range map[string]func(*Options){
		"empty provider": func(o *Options) { o.Judge.Provider = "" },
		"empty base url": func(o *Options) { o.Judge.BaseURL = "" },
		"empty model":    func(o *Options) { o.Judge.Model = "" },
		"empty api key":  func(o *Options) { o.Judge.APIKeyEnv = "" },
		"nil lookup":     func(o *Options) { o.APIKeyLookup = nil },
	} {
		t.Run(name, func(t *testing.T) {
			options := base
			mutate(&options)
			if _, err := New(options); err == nil {
				t.Fatalf("New() accepted invalid options %#v", options)
			}
		})
	}
}

func TestPrepareSandboxRemovesThenProvesAnswerPathAbsent(t *testing.T) {
	benchmark := &Benchmark{details: map[string]taskDetails{"task": {}}}
	sandbox := &prepareSandboxFake{}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "task"}, sandbox); err != nil {
		t.Fatal(err)
	}
	if len(sandbox.commands) != 2 {
		t.Fatalf("commands = %#v", sandbox.commands)
	}
	remove := sandbox.commands[0]
	if remove.Path != "/bin/rm" || !reflect.DeepEqual(remove.Args, []string{"-rf", "--", answerPath}) {
		t.Fatalf("remove command = %#v", remove)
	}
	probe := sandbox.commands[1]
	if probe.Path != "/bin/sh" || len(probe.Args) != 4 || probe.Args[0] != "-c" ||
		!strings.Contains(probe.Args[1], `! -e "$path"`) || !strings.Contains(probe.Args[1], `! -L "$path"`) ||
		probe.Args[2] != "aries-answer-absence" || probe.Args[3] != answerPath {
		t.Fatalf("absence probe = %#v", probe)
	}
	if sandbox.uploads != 0 {
		t.Fatalf("preparation uploaded %d files", sandbox.uploads)
	}
}

func TestPrepareSandboxFailsClosed(t *testing.T) {
	benchmark := &Benchmark{details: map[string]taskDetails{"task": {}}}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "task"}, nil); err == nil {
		t.Fatal("nil sandbox accepted")
	}
	if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "missing"}, &prepareSandboxFake{}); err == nil {
		t.Fatal("unloaded task accepted")
	}
	for name, sandbox := range map[string]*prepareSandboxFake{
		"remove error":   {errors: []error{errors.New("remove")}},
		"remove nonzero": {results: []core.CommandResult{{ExitCode: 2}}},
		"probe error":    {errors: []error{nil, errors.New("probe")}},
		"dangling link":  {results: []core.CommandResult{{}, {ExitCode: 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := benchmark.PrepareSandbox(context.Background(), core.Task{ID: "task"}, sandbox); err == nil {
				t.Fatal("expected failure")
			}
			if sandbox.uploads != 0 {
				t.Fatal("preparation uploaded verifier files")
			}
		})
	}
}

func TestNewRejectsUnsafeAndDuplicateTasks(t *testing.T) {
	for _, test := range []struct {
		name string
		ids  []string
		want string
	}{
		{"traversal", []string{"../task"}, "invalid"},
		{"space", []string{"bad task"}, "invalid"},
		{"control character", []string{"bad\ntask"}, "invalid"},
		{"duplicate", []string{qaTaskID, qaTaskID}, "duplicate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := testOptions("root", test.ids, "runs")
			if _, err := New(options); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want %q", err, test.want)
			}
		})
	}
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
		return fixtureRevision
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type prepareSandboxFake struct {
	commands []core.Command
	results  []core.CommandResult
	errors   []error
	uploads  int
}

func (s *prepareSandboxFake) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	index := len(s.commands)
	s.commands = append(s.commands, command)
	var result core.CommandResult
	if index < len(s.results) {
		result = s.results[index]
	}
	var err error
	if index < len(s.errors) {
		err = s.errors[index]
	}
	return result, err
}

func (s *prepareSandboxFake) Upload(context.Context, string, string) error {
	s.uploads++
	return nil
}

func (*prepareSandboxFake) Download(context.Context, string, string) error { return nil }
