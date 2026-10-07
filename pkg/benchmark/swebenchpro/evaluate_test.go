package swebenchpro

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	evaluationCandidatePatch = "diff --git a/source.py b/source.py\n--- a/source.py\n+++ b/source.py\n@@ -1 +1 @@\n-old\n+new\n"
	evaluationBinaryPatch    = "diff --git a/image.bin b/image.bin\nnew file mode 100644\nindex 0000000..1111111\nGIT binary patch\nliteral 1\nA0000\n\n"
	evaluationPassingOutput  = `{"tests":[{"name":"tests.regression_test::test_fix","status":"PASSED"},{"name":"tests.existing_test::test_ok","status":"PASSED"}]}`
)

func TestEvaluateCapturesPatchThenRunsUpstreamEntryScriptInFreshSandbox(t *testing.T) {
	benchmark, task, details := newEvaluationFixture(t)
	agent := &agentSandboxFake{candidate: evaluationCandidatePatch + evaluationBinaryPatch}
	fresh := &freshSandboxFake{
		output:     evaluationPassingOutput + "\n",
		testResult: core.CommandResult{ExitCode: 7, Stdout: "test stdout\n", Stderr: "test stderr\n"},
	}
	sandboxes := &sandboxesFake{fresh: fresh}
	evaluation, err := benchmark.Evaluate(context.Background(), task, agent, sandboxes)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Status != core.StatusSucceeded || evaluation.VerifierStatus != core.StatusSucceeded || evaluation.Score != 1 || evaluation.Reward != 1 {
		t.Fatalf("evaluation = %+v", evaluation)
	}

	// The agent sandbox only yields the patch, captured as the agent user.
	wantAgent := []commandShape{
		{path: "/bin/sh", args: []string{"-c", quiesceAgentPredicate, "aries-swebenchpro-quiesce", agentUID}, user: rootExecUser},
		{path: "/usr/bin/git", args: []string{"-C", repositoryPath, "add", "-A"}, user: agentExecUser},
		{path: "/usr/bin/git", args: []string{"-C", repositoryPath, "diff", "--cached", "--no-ext-diff", "--binary", details.baseCommit}, user: agentExecUser, limit: maxCandidatePatchSize},
	}
	if got := shapes(agent.commands); !reflect.DeepEqual(got, wantAgent) {
		t.Fatalf("agent commands = %#v, want %#v", got, wantAgent)
	}
	if agent.uploads != 0 || agent.downloads != 0 {
		t.Fatalf("agent sandbox transfers: uploads=%d downloads=%d", agent.uploads, agent.downloads)
	}

	// The fresh sandbox comes from the task image without the agent identity.
	if len(sandboxes.environments) != 1 {
		t.Fatalf("fresh sandboxes started = %d, want 1", len(sandboxes.environments))
	}
	wantEnvironment := task.Environment
	wantEnvironment.ExecUser = ""
	if !reflect.DeepEqual(sandboxes.environments[0], wantEnvironment) {
		t.Fatalf("fresh environment = %#v, want %#v", sandboxes.environments[0], wantEnvironment)
	}
	wantFresh := []commandShape{
		{path: "/bin/mkdir", args: []string{"-p", "--", workspaceContainerPath}},
		{path: "/usr/bin/git", args: []string{"reset", "--hard", details.baseCommit}, dir: repositoryPath},
		{path: "/usr/bin/git", args: []string{"checkout", details.baseCommit}, dir: repositoryPath},
		{path: "/usr/bin/git", args: []string{"apply", "-v", patchContainerPath}, dir: repositoryPath},
		{path: "/usr/bin/git", args: append([]string{"checkout", details.goldCommit, "--"}, details.verifierFiles...), dir: repositoryPath},
		{path: "/bin/bash", args: []string{runScriptContainerPath, strings.Join(details.selectedTests, ",")}, dir: repositoryPath, limit: maxVerifierLogSize, timeout: true},
		{path: "/usr/bin/env", args: []string{"python", parserContainerPath, stdoutContainerPath, stderrContainerPath, outputContainerPath}, dir: repositoryPath, timeout: true},
	}
	if got := shapes(fresh.commands); !reflect.DeepEqual(got, wantFresh) {
		t.Fatalf("fresh commands = %#v, want %#v", got, wantFresh)
	}
	wantUploads := []string{patchContainerPath, runScriptContainerPath, parserContainerPath, stdoutContainerPath, stderrContainerPath}
	if !reflect.DeepEqual(fresh.uploadDestinations, wantUploads) {
		t.Fatalf("uploads = %v, want %v", fresh.uploadDestinations, wantUploads)
	}
	if fresh.uploadedPatch != evaluationCandidatePatch {
		t.Fatalf("uploaded patch = %q, want binary hunks stripped", fresh.uploadedPatch)
	}

	if len(evaluation.LogPaths) != len(evaluationArtifactNames) {
		t.Fatalf("log paths = %v", evaluation.LogPaths)
	}
	for index, want := range []string{evaluationCandidatePatch + evaluationBinaryPatch, evaluationCandidatePatch, "test stdout\n", "test stderr\n", evaluationPassingOutput + "\n"} {
		if content, err := os.ReadFile(evaluation.LogPaths[index]); err != nil || string(content) != want {
			t.Fatalf("artifact %s = %q, %v; want %q", evaluation.LogPaths[index], content, err, want)
		}
	}
	if reason, err := os.ReadFile(evaluation.LogPaths[5]); err != nil || string(reason) != "resolved: all FAIL_TO_PASS and PASS_TO_PASS tests passed\n" {
		t.Fatalf("reason = %q, %v", reason, err)
	}
	for _, path := range evaluation.LogPaths {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact %q = %#v, %v", path, info, err)
		}
	}
}

func TestEvaluateUnresolvedAndDuplicateParserResultsAreCompatible(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	fresh := &freshSandboxFake{output: `{"tests":[` +
		`{"name":"tests.regression_test::test_fix","status":"FAILED"},` +
		`{"name":"tests.regression_test::test_fix","status":"PASSED"},` +
		`{"name":"tests.existing_test::test_ok","status":"SKIPPED"}]}`}
	evaluation, err := benchmark.Evaluate(context.Background(), task, &agentSandboxFake{candidate: evaluationCandidatePatch}, &sandboxesFake{fresh: fresh})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Status != core.StatusFailed || evaluation.VerifierStatus != core.StatusFailed || evaluation.Score != 0 || evaluation.Reward != 0 {
		t.Fatalf("evaluation = %+v", evaluation)
	}
	reason, err := os.ReadFile(evaluation.LogPaths[5])
	if err != nil || !strings.Contains(string(reason), "tests.existing_test::test_ok") {
		t.Fatalf("reason = %q, %v", reason, err)
	}
}

func TestEvaluateRunsTestsAfterCandidateApplyFailureLikeUpstream(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	fresh := &freshSandboxFake{
		output:      `{"tests":[{"name":"tests.existing_test::test_ok","status":"PASSED"}]}`,
		applyResult: core.CommandResult{ExitCode: 1, Stderr: "does not apply"},
	}
	evaluation, err := benchmark.Evaluate(context.Background(), task, &agentSandboxFake{candidate: evaluationCandidatePatch}, &sandboxesFake{fresh: fresh})
	if err != nil {
		t.Fatalf("candidate apply failure returned plumbing error: %v", err)
	}
	if evaluation.Reward != 0 || evaluation.Status != core.StatusFailed {
		t.Fatalf("evaluation = %+v", evaluation)
	}
	if !fresh.parserRan {
		t.Fatal("verifier did not run after the patch failed to apply")
	}
	reason, err := os.ReadFile(evaluation.LogPaths[5])
	if err != nil || !strings.Contains(string(reason), "candidate patch did not apply: exit code 1: does not apply") ||
		!strings.Contains(string(reason), "unresolved: missing required passing tests: tests.regression_test::test_fix") {
		t.Fatalf("reason = %q, %v", reason, err)
	}
}

func TestEvaluateUsesEmptyPatchWhenCaptureFails(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	agent := &agentSandboxFake{candidate: evaluationCandidatePatch, stageResult: core.CommandResult{ExitCode: 128, Stderr: "not a git repository"}}
	fresh := &freshSandboxFake{output: evaluationPassingOutput}
	evaluation, err := benchmark.Evaluate(context.Background(), task, agent, &sandboxesFake{fresh: fresh})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.uploadedPatch != "" {
		t.Fatalf("uploaded patch = %q, want empty", fresh.uploadedPatch)
	}
	for _, command := range agent.commands {
		if slicesEqual(command.Args[2:3], []string{"diff"}) {
			t.Fatalf("diff ran after staging failed: %#v", command)
		}
	}
	reason, err := os.ReadFile(evaluation.LogPaths[5])
	if err != nil || !strings.Contains(string(reason), "candidate patch could not be captured (git stage exit code 128); evaluating an empty patch: not a git repository") {
		t.Fatalf("reason = %q, %v", reason, err)
	}
}

func TestEvaluateRejectsMalformedParserOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{name: "trailing JSON", output: `{"tests":[]} {}`},
		{name: "missing tests", output: `{}`},
		{name: "null tests", output: `{"tests":null}`},
		{name: "missing status", output: `{"tests":[{"name":"test"}]}`},
		{name: "non-string status", output: `{"tests":[{"name":"test","status":1}]}`},
		{name: "empty name", output: `{"tests":[{"name":"","status":"PASSED"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			benchmark, task, _ := newEvaluationFixture(t)
			fresh := &freshSandboxFake{output: test.output}
			if _, err := benchmark.Evaluate(context.Background(), task, &agentSandboxFake{candidate: evaluationCandidatePatch}, &sandboxesFake{fresh: fresh}); err == nil {
				t.Fatal("malformed parser output accepted")
			}
		})
	}
}

func TestEvaluateFailsParserErrorAsEvaluatorError(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	fresh := &freshSandboxFake{output: evaluationPassingOutput, parserResult: core.CommandResult{ExitCode: 1}}
	if _, err := benchmark.Evaluate(context.Background(), task, &agentSandboxFake{candidate: evaluationCandidatePatch}, &sandboxesFake{fresh: fresh}); err == nil ||
		!strings.Contains(err.Error(), "run SWE-bench Pro parser: exit code 1") {
		t.Fatalf("parser failure error = %v", err)
	}
}

func TestEvaluateFailsBeforeSandboxesForInvalidInputsAndSources(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	agent := &agentSandboxFake{}
	sandboxes := &sandboxesFake{fresh: &freshSandboxFake{}}
	if _, err := benchmark.Evaluate(context.Background(), task, nil, sandboxes); err == nil {
		t.Fatal("nil task sandbox accepted")
	}
	if _, err := benchmark.Evaluate(context.Background(), task, agent, nil); err == nil {
		t.Fatal("missing evaluation sandboxes accepted")
	}
	if _, err := benchmark.Evaluate(context.Background(), core.Task{ID: "missing"}, agent, sandboxes); err == nil {
		t.Fatal("unloaded task accepted")
	}
	if err := os.WriteFile(filepath.Join(benchmark.datasetRoot(), "dirty"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Evaluate(context.Background(), task, agent, sandboxes); err == nil {
		t.Fatal("dirty source accepted")
	}
	if len(agent.commands) != 0 || len(sandboxes.environments) != 0 {
		t.Fatalf("invalid inputs reached sandboxes: agent=%v fresh=%d", agent.commands, len(sandboxes.environments))
	}
}

func TestEvaluateReportsFreshSandboxStartFailure(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	failure := errors.New("image pull failed")
	_, err := benchmark.Evaluate(context.Background(), task, &agentSandboxFake{candidate: evaluationCandidatePatch}, &sandboxesFake{err: failure})
	if !errors.Is(err, failure) || !strings.Contains(err.Error(), "start fresh SWE-bench Pro evaluation sandbox") {
		t.Fatalf("start failure error = %v", err)
	}
}

func TestEvaluateRequiresSandboxCapabilities(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	basic := &basicSandboxFake{}
	sandboxes := &sandboxesFake{fresh: &freshSandboxFake{}}
	if _, err := benchmark.Evaluate(context.Background(), task, basic, sandboxes); err == nil || !strings.Contains(err.Error(), "streaming sandbox execution") || basic.calls != 0 {
		t.Fatalf("basic task sandbox error = %v, calls = %d", err, basic.calls)
	}
	freshBasic := &basicSandboxFake{}
	if _, err := benchmark.Evaluate(context.Background(), task, &agentSandboxFake{candidate: evaluationCandidatePatch}, &sandboxesFake{fresh: freshBasic}); err == nil ||
		!strings.Contains(err.Error(), "bounded sandbox downloads") || freshBasic.calls != 0 {
		t.Fatalf("basic fresh sandbox error = %v, calls = %d", err, freshBasic.calls)
	}
}

func TestEvaluateClearsStaleArtifacts(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	artifactDir := filepath.Join(benchmark.outputDir, task.ID, "evaluation")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range evaluationArtifactNames {
		if err := os.WriteFile(filepath.Join(artifactDir, name), []byte("STALE PRIVATE DATA"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	evaluation, err := benchmark.Evaluate(context.Background(), task, &agentSandboxFake{candidate: evaluationCandidatePatch}, &sandboxesFake{fresh: &freshSandboxFake{output: evaluationPassingOutput}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range evaluation.LogPaths {
		content, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(content), "STALE PRIVATE DATA") {
			t.Fatalf("artifact %q = %q, %v", path, content, err)
		}
	}
}

func TestStripBinaryPatchSections(t *testing.T) {
	input := []byte("preamble\n" + evaluationBinaryPatch + evaluationCandidatePatch +
		"diff --git a/old.bin b/old.bin\nindex 1111111..2222222 100644\nBinary files a/old.bin and b/old.bin differ\n")
	got, err := stripBinaryPatchSections(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "preamble\n"+evaluationCandidatePatch {
		t.Fatalf("stripped patch = %q", got)
	}
}

type commandShape struct {
	path    string
	args    []string
	dir     string
	user    string
	limit   int
	timeout bool
}

func shapes(commands []core.Command) []commandShape {
	result := make([]commandShape, len(commands))
	for index, command := range commands {
		result[index] = commandShape{path: command.Path, args: command.Args, dir: command.Dir, user: command.User, limit: command.OutputLimitBytes, timeout: command.Timeout == defaultVerifierTimeout}
	}
	return result
}

type sandboxesFake struct {
	fresh        runner.Sandbox
	err          error
	environments []core.Environment
}

func (s *sandboxesFake) Start(_ context.Context, environment core.Environment) (runner.Sandbox, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.environments = append(s.environments, environment)
	return s.fresh, nil
}

type basicSandboxFake struct{ calls int }

func (s *basicSandboxFake) Exec(context.Context, core.Command) (core.CommandResult, error) {
	s.calls++
	return core.CommandResult{}, nil
}
func (s *basicSandboxFake) Upload(context.Context, string, string) error {
	s.calls++
	return nil
}
func (s *basicSandboxFake) Download(context.Context, string, string) error {
	s.calls++
	return nil
}
func (s *basicSandboxFake) Connectivity() core.HarnessConnectivity { return core.HarnessConnectivity{} }

type agentSandboxFake struct {
	candidate   string
	stageResult core.CommandResult
	commands    []core.Command
	uploads     int
	downloads   int
}

func (s *agentSandboxFake) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	if command.Path == "/usr/bin/git" && slicesEqual(command.Args[2:], []string{"add", "-A"}) {
		return s.stageResult, nil
	}
	return core.CommandResult{}, nil
}

func (s *agentSandboxFake) ExecStream(_ context.Context, command core.Command, _ io.Reader, stdout, _ io.Writer) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	_, err := io.WriteString(stdout, s.candidate)
	return core.CommandResult{}, err
}

func (s *agentSandboxFake) Upload(context.Context, string, string) error {
	s.uploads++
	return nil
}

func (s *agentSandboxFake) Download(context.Context, string, string) error {
	s.downloads++
	return nil
}

func (s *agentSandboxFake) Connectivity() core.HarnessConnectivity {
	return core.HarnessConnectivity{Placement: core.RuntimePlacement{DockerNetwork: "agent-network"}}
}

type freshSandboxFake struct {
	output             string
	applyResult        core.CommandResult
	parserResult       core.CommandResult
	testResult         core.CommandResult
	commands           []core.Command
	uploadDestinations []string
	uploadedPatch      string
	parserRan          bool
}

func (s *freshSandboxFake) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	switch {
	case command.Path == "/usr/bin/git" && len(command.Args) > 0 && command.Args[0] == "apply":
		return s.applyResult, nil
	case command.Path == "/usr/bin/env":
		s.parserRan = true
		return s.parserResult, nil
	}
	return core.CommandResult{}, nil
}

func (s *freshSandboxFake) ExecStream(_ context.Context, command core.Command, _ io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	_, _ = io.WriteString(stdout, s.testResult.Stdout)
	_, _ = io.WriteString(stderr, s.testResult.Stderr)
	return s.testResult, nil
}

func (s *freshSandboxFake) Upload(_ context.Context, source, destination string) error {
	s.uploadDestinations = append(s.uploadDestinations, destination)
	if destination == patchContainerPath {
		content, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		s.uploadedPatch = string(content)
	}
	return nil
}

func (s *freshSandboxFake) Download(context.Context, string, string) error {
	return errors.New("evaluation must use bounded downloads")
}

func (s *freshSandboxFake) DownloadLimit(_ context.Context, source, destination string, _ int64) error {
	if source != outputContainerPath {
		return errors.New("unexpected download")
	}
	if !s.parserRan {
		return errors.New("parser output requested before parser ran")
	}
	return os.WriteFile(destination, []byte(s.output), 0o600)
}

func (s *freshSandboxFake) Connectivity() core.HarnessConnectivity {
	return core.HarnessConnectivity{Placement: core.RuntimePlacement{DockerNetwork: "evaluation-network"}}
}

func newEvaluationFixture(t *testing.T) (*Benchmark, core.Task, taskDetails) {
	t.Helper()
	root := t.TempDir()
	datasetRoot := filepath.Join(root, "dataset")
	evaluatorRoot := filepath.Join(root, "evaluator")
	for _, directory := range []string{datasetRoot, evaluatorRoot} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runScript := filepath.Join(evaluatorRoot, runScriptsDirectory, fixtureInstanceID, runScriptName)
	parser := filepath.Join(evaluatorRoot, runScriptsDirectory, fixtureInstanceID, parserFileName)
	writeTestFile(t, runScript, "#!/bin/bash\n")
	writeTestFile(t, parser, "# parser\n")
	writeTestFile(t, filepath.Join(datasetRoot, "README.md"), "dataset\n")
	datasetRevision := commitEvaluationFixture(t, datasetRoot)
	evaluatorRevision := commitEvaluationFixture(t, evaluatorRoot)
	details := taskDetails{
		baseCommit:    strings.Repeat("a", 40),
		goldCommit:    strings.Repeat("b", 40),
		testPatch:     fixtureTestPatch,
		failToPass:    []string{"tests.regression_test::test_fix"},
		passToPass:    []string{"tests.existing_test::test_ok"},
		selectedTests: []string{"tests/regression_test.py", "tests/existing_test.py"},
		verifierFiles: []string{"tests/regression_test.py"},
		runScript:     runScript,
		parser:        parser,
	}
	benchmark := &Benchmark{
		root: root, outputDir: t.TempDir(), datasetRevision: datasetRevision, evaluatorRevision: evaluatorRevision,
		details: map[string]taskDetails{fixtureInstanceID: details},
	}
	task := core.Task{ID: fixtureInstanceID, Environment: core.Environment{
		Image: "docker.io/jefzda/sweap-images:fixture", Workdir: repositoryPath, CPU: 4, MemoryMB: 1024,
		AllowNetwork: true, ExecUser: agentExecUser,
	}}
	return benchmark, task, details
}

func commitEvaluationFixture(t *testing.T, root string) string {
	t.Helper()
	commands := [][]string{
		{"init", "--quiet"},
		{"add", "-A"},
		{"-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "commit", "--quiet", "-m", "fixture"},
	}
	for _, arguments := range commands {
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	output, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}
