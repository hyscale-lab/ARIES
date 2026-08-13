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
)

const (
	evaluationCandidatePatch = "diff --git a/source.py b/source.py\n--- a/source.py\n+++ b/source.py\n@@ -1 +1 @@\n-old\n+new\n"
	evaluationBinaryPatch    = "diff --git a/image.bin b/image.bin\nnew file mode 100644\nindex 0000000..1111111\nGIT binary patch\nliteral 1\nA0000\n\n"
)

func TestEvaluateResolvedUsesPinnedPrivateVerifierAfterCandidateCapture(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	sandbox := &evaluationSandboxFake{
		candidate: []byte(evaluationCandidatePatch + evaluationBinaryPatch),
		output:    []byte(`{"tests":[{"name":"tests.regression_test::test_fix","status":"PASSED"},{"name":"tests.existing_test::test_ok","status":"PASSED"},{"name":"extra","status":"UNKNOWN"}]}`),
		commandResults: map[string]core.CommandResult{
			"/bin/bash": {ExitCode: 7, Stdout: "test stdout\n", Stderr: "test stderr\n"},
		},
	}
	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Status != core.StatusSucceeded || evaluation.VerifierStatus != core.StatusSucceeded || evaluation.Score != 1 || evaluation.Reward != 1 {
		t.Fatalf("evaluation = %+v", evaluation)
	}
	if len(evaluation.LogPaths) != 6 {
		t.Fatalf("log paths = %v", evaluation.LogPaths)
	}
	raw, err := os.ReadFile(evaluation.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	effective, err := os.ReadFile(evaluation.LogPaths[1])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != evaluationCandidatePatch+evaluationBinaryPatch || string(effective) != evaluationCandidatePatch {
		t.Fatalf("raw/effective patches = %q / %q", raw, effective)
	}
	if content, err := os.ReadFile(evaluation.LogPaths[2]); err != nil || string(content) != "test stdout\n" {
		t.Fatalf("stdout = %q, %v", content, err)
	}
	if content, err := os.ReadFile(evaluation.LogPaths[3]); err != nil || string(content) != "test stderr\n" {
		t.Fatalf("stderr = %q, %v", content, err)
	}
	for _, path := range evaluation.LogPaths {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("artifact %q = %#v, %v", path, info, err)
		}
	}

	var testCommand, parserCommand *core.Command
	for index := range sandbox.commands {
		command := &sandbox.commands[index]
		if command.Path == "/bin/bash" && len(command.Args) > 0 && command.Args[0] == runScriptContainerPath {
			testCommand = command
			continue
		}
		if command.Path == "/usr/bin/env" {
			parserCommand = command
		}
		if command.User != rootExecUser {
			t.Fatalf("privileged evaluator command ran as %q: %#v", command.User, command)
		}
	}
	if testCommand == nil || testCommand.User != agentExecUser || testCommand.Timeout != defaultVerifierTimeout || testCommand.OutputLimitBytes != maxVerifierLogSize {
		t.Fatalf("verifier command = %#v", testCommand)
	}
	if parserCommand == nil || parserCommand.User != rootExecUser || parserCommand.Dir != "/" ||
		parserCommand.Timeout != defaultVerifierTimeout || !slicesEqual(parserCommand.Args[:5], []string{"-i", "PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONNOUSERSITE=1", "python", "-I"}) {
		t.Fatalf("isolated parser command = %#v", parserCommand)
	}
	if len(sandbox.commands) < 2 || sandbox.commands[0].Args[1] != quiesceAgentPredicate ||
		sandbox.commands[len(sandbox.commands)-2].Path != "/bin/rm" || sandbox.commands[len(sandbox.commands)-1].Args[1] != absencePredicate {
		t.Fatalf("process isolation or final staging cleanup missing: %#v", shapes(sandbox.commands))
	}
	wantUploadDestinations := []string{
		evaluationGitSnapshotContainerPath,
		evaluationIgnoredSnapshotContainerPath,
		candidateEffectiveContainerPath,
		verifierSnapshotContainerPath,
		runScriptContainerPath,
		parserContainerPath,
		verifierStdoutContainerPath,
		verifierStderrContainerPath,
	}
	if !reflect.DeepEqual(sandbox.uploadDestinations, wantUploadDestinations) {
		t.Fatalf("uploads = %v, want %v", sandbox.uploadDestinations, wantUploadDestinations)
	}
	for _, source := range sandbox.uploadSources {
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), fixtureTestPatch) || strings.Contains(string(content), fixtureGoldPatch) {
			t.Fatalf("uploaded source %q exposed dataset patch material", source)
		}
	}
	if sandbox.downloadSources[0] != candidateRawContainerPath || sandbox.downloadSources[1] != parserOutputContainerPath {
		t.Fatalf("downloads = %v", sandbox.downloadSources)
	}
}

func TestEvaluateUnresolvedAndDuplicateParserResultsAreCompatible(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	sandbox := &evaluationSandboxFake{
		candidate: []byte(evaluationCandidatePatch),
		output: []byte(`{"tests":[` +
			`{"name":"tests.regression_test::test_fix","status":"FAILED"},` +
			`{"name":"tests.regression_test::test_fix","status":"PASSED"},` +
			`{"name":"tests.existing_test::test_ok","status":"SKIPPED"}]}`),
	}
	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Status != core.StatusFailed || evaluation.VerifierStatus != core.StatusFailed || evaluation.Score != 0 || evaluation.Reward != 0 {
		t.Fatalf("evaluation = %+v", evaluation)
	}
	reason, err := os.ReadFile(evaluation.LogPaths[5])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reason), "tests.existing_test::test_ok") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestEvaluateCandidateApplyFailureIsAZeroRewardOutcome(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	sandbox := &evaluationSandboxFake{
		candidate: []byte(evaluationCandidatePatch),
		commandResults: map[string]core.CommandResult{
			"git apply": {ExitCode: 1, Stderr: "does not apply"},
		},
	}
	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatalf("candidate apply failure returned plumbing error: %v", err)
	}
	if evaluation.Reward != 0 || evaluation.Status != core.StatusFailed {
		t.Fatalf("evaluation = %+v", evaluation)
	}
	for _, command := range sandbox.commands {
		if command.Path == "/usr/bin/env" || slicesEqual(command.Args, []string{"-xf", verifierSnapshotContainerPath, "-C", repositoryPath}) {
			t.Fatalf("rejected candidate reached private verifier execution: %#v", command)
		}
	}
	if sandbox.commands[len(sandbox.commands)-2].Path != "/bin/rm" || sandbox.commands[len(sandbox.commands)-1].Args[1] != absencePredicate {
		t.Fatalf("rejected candidate did not scrub evaluator staging: %#v", shapes(sandbox.commands))
	}
	reason, err := os.ReadFile(evaluation.LogPaths[5])
	if err != nil || !strings.Contains(string(reason), "candidate patch did not apply") {
		t.Fatalf("reason = %q, %v", reason, err)
	}
	for _, path := range evaluation.LogPaths {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("rejected candidate artifact %q = %#v, %v", path, info, err)
		}
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
			sandbox := &evaluationSandboxFake{candidate: []byte(evaluationCandidatePatch), output: []byte(test.output)}
			if _, err := benchmark.Evaluate(context.Background(), task, sandbox); err == nil {
				t.Fatal("malformed parser output accepted")
			}
		})
	}
}

func TestEvaluateFailsBeforeSandboxForInvalidInputsAndSources(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	if _, err := benchmark.Evaluate(context.Background(), task, nil); err == nil {
		t.Fatal("nil sandbox accepted")
	}
	sandbox := &evaluationSandboxFake{}
	if _, err := benchmark.Evaluate(context.Background(), core.Task{ID: "missing"}, sandbox); err == nil || len(sandbox.commands) != 0 {
		t.Fatalf("unloaded task error = %v, commands = %v", err, sandbox.commands)
	}
	if err := os.WriteFile(filepath.Join(benchmark.datasetRoot(), "dirty"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := benchmark.Evaluate(context.Background(), task, sandbox); err == nil || len(sandbox.commands) != 0 {
		t.Fatalf("dirty source error = %v, commands = %v", err, sandbox.commands)
	}
}

func TestEvaluateRejectsMissingOrSymlinkSnapshotBeforeSandbox(t *testing.T) {
	for _, snapshotKind := range []string{"verifier", "ignored"} {
		for _, symlink := range []bool{false, true} {
			t.Run(snapshotKind+"/"+map[bool]string{false: "missing", true: "symlink"}[symlink], func(t *testing.T) {
				benchmark, task, details := newEvaluationFixture(t)
				path := details.snapshot
				if snapshotKind == "ignored" {
					path = details.ignoredSnapshot
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if symlink {
					target := filepath.Join(t.TempDir(), "target.tar")
					if err := os.WriteFile(target, []byte("private snapshot"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				}
				sandbox := &evaluationSandboxFake{}
				if _, err := benchmark.Evaluate(context.Background(), task, sandbox); err == nil || len(sandbox.commands) != 0 {
					t.Fatalf("snapshot error = %v, commands = %v", err, sandbox.commands)
				}
			})
		}
	}
}

func TestEvaluateRejectsOversizedIgnoredSnapshotBeforeSandbox(t *testing.T) {
	benchmark, task, details := newEvaluationFixture(t)
	if err := os.Truncate(details.ignoredSnapshot, maxIgnoredSnapshotSize+1); err != nil {
		t.Fatal(err)
	}
	sandbox := &evaluationSandboxFake{}
	if _, err := benchmark.Evaluate(context.Background(), task, sandbox); err == nil || len(sandbox.commands) != 0 {
		t.Fatalf("oversized ignored snapshot error = %v, commands = %v", err, sandbox.commands)
	}
}

func TestEvaluateClearsStaleArtifactsAndContainerOutput(t *testing.T) {
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
	sandbox := &evaluationSandboxFake{
		candidate: []byte(evaluationCandidatePatch),
		output: []byte(`{"tests":[{"name":"tests.regression_test::test_fix","status":"PASSED"},` +
			`{"name":"tests.existing_test::test_ok","status":"PASSED"}]}`),
	}
	evaluation, err := benchmark.Evaluate(context.Background(), task, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range evaluation.LogPaths {
		content, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(content), "STALE PRIVATE DATA") {
			t.Fatalf("artifact %q = %q, %v", path, content, err)
		}
	}
	if len(sandbox.commands) < 4 || sandbox.commands[0].Args[1] != quiesceAgentPredicate ||
		sandbox.commands[1].Path != "/bin/rm" || sandbox.commands[2].Args[1] != absencePredicate ||
		sandbox.commands[3].Path != "/bin/mkdir" {
		t.Fatalf("initial cleanup commands = %#v", shapes(sandbox.commands))
	}
}

func TestEvaluateRejectsVerifierSymlinkTraversalBeforeExtraction(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	sandbox := &evaluationSandboxFake{
		candidate: []byte(evaluationCandidatePatch),
		predicateResults: map[string]core.CommandResult{
			installVerifierPredicate: {ExitCode: 1},
		},
	}
	if _, err := benchmark.Evaluate(context.Background(), task, sandbox); err == nil {
		t.Fatal("symlink-unsafe verifier destination was accepted")
	}
	for _, command := range sandbox.commands {
		if command.Path == "/usr/bin/env" || slicesEqual(command.Args, []string{"-xf", verifierSnapshotContainerPath, "-C", repositoryPath}) {
			t.Fatalf("private verifier reached extraction or parser after destination rejection: %#v", command)
		}
	}
	if sandbox.commands[len(sandbox.commands)-2].Path != "/bin/rm" || sandbox.commands[len(sandbox.commands)-1].Args[1] != absencePredicate {
		t.Fatalf("failed verifier install did not scrub private staging: %#v", shapes(sandbox.commands))
	}
}

func TestEvaluateRequiresBoundedDownloadAndStreamingBeforeSandbox(t *testing.T) {
	benchmark, task, _ := newEvaluationFixture(t)
	basic := &basicEvaluationSandboxFake{}
	if _, err := benchmark.Evaluate(context.Background(), task, basic); err == nil || !strings.Contains(err.Error(), "bounded sandbox downloads") || basic.calls != 0 {
		t.Fatalf("basic sandbox error = %v, calls = %d", err, basic.calls)
	}
	limited := &limitedEvaluationSandboxFake{basicEvaluationSandboxFake: &basicEvaluationSandboxFake{}}
	if _, err := benchmark.Evaluate(context.Background(), task, limited); err == nil || !strings.Contains(err.Error(), "streaming sandbox execution") || limited.calls != 0 {
		t.Fatalf("limited-only sandbox error = %v, calls = %d", err, limited.calls)
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
	path string
	args []string
	dir  string
}

func shapes(commands []core.Command) []commandShape {
	result := make([]commandShape, len(commands))
	for index, command := range commands {
		result[index] = commandShape{path: command.Path, args: command.Args, dir: command.Dir}
	}
	return result
}

type basicEvaluationSandboxFake struct{ calls int }

func (s *basicEvaluationSandboxFake) Exec(context.Context, core.Command) (core.CommandResult, error) {
	s.calls++
	return core.CommandResult{}, nil
}
func (s *basicEvaluationSandboxFake) Upload(context.Context, string, string) error {
	s.calls++
	return nil
}
func (s *basicEvaluationSandboxFake) Download(context.Context, string, string) error {
	s.calls++
	return nil
}

type limitedEvaluationSandboxFake struct{ *basicEvaluationSandboxFake }

func (s *limitedEvaluationSandboxFake) DownloadLimit(context.Context, string, string, int64) error {
	s.calls++
	return nil
}

type evaluationSandboxFake struct {
	candidate          []byte
	output             []byte
	commands           []core.Command
	commandResults     map[string]core.CommandResult
	commandErrors      map[string]error
	predicateResults   map[string]core.CommandResult
	uploadSources      []string
	uploadDestinations []string
	downloadSources    []string
	parserRan          bool
}

func (s *evaluationSandboxFake) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	key := command.Path
	if command.Path == "/bin/sh" && len(command.Args) >= 2 {
		if result, ok := s.predicateResults[command.Args[1]]; ok {
			return result, nil
		}
	}
	if command.Path == "/usr/bin/git" && len(command.Args) >= 3 {
		switch command.Args[2] {
		case "rev-parse":
			return core.CommandResult{Stdout: strings.Repeat("a", 40) + "\n"}, nil
		case "cat-file":
			return core.CommandResult{ExitCode: 1}, nil
		}
	}
	if command.Path == "/usr/bin/git" && len(command.Args) >= 4 && command.Args[2] == "apply" {
		key = "git apply"
	}
	if command.Path == "/usr/bin/env" {
		s.parserRan = true
	}
	return s.commandResults[key], s.commandErrors[key]
}

func (s *evaluationSandboxFake) ExecStream(_ context.Context, command core.Command, _ io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	result := s.commandResults[command.Path]
	if stdout != nil {
		_, _ = io.WriteString(stdout, result.Stdout)
	}
	if stderr != nil {
		_, _ = io.WriteString(stderr, result.Stderr)
	}
	return result, s.commandErrors[command.Path]
}

func (s *evaluationSandboxFake) Upload(_ context.Context, source, destination string) error {
	s.uploadSources = append(s.uploadSources, source)
	s.uploadDestinations = append(s.uploadDestinations, destination)
	return nil
}

func (s *evaluationSandboxFake) Download(_ context.Context, source, destination string) error {
	s.downloadSources = append(s.downloadSources, source)
	switch source {
	case candidateRawContainerPath:
		return os.WriteFile(destination, s.candidate, 0o600)
	case parserOutputContainerPath:
		if !s.parserRan {
			return errors.New("parser output requested before parser ran")
		}
		return os.WriteFile(destination, s.output, 0o600)
	default:
		return errors.New("unexpected download")
	}
}

func (s *evaluationSandboxFake) DownloadLimit(ctx context.Context, source, destination string, _ int64) error {
	return s.Download(ctx, source, destination)
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
	output := t.TempDir()
	snapshot := filepath.Join(output, fixtureInstanceID, "private", "verifier-tests.tar")
	ignoredSnapshot := filepath.Join(output, fixtureInstanceID, "private", "ignored-baseline.tar")
	gitSnapshot := filepath.Join(output, fixtureInstanceID, "private", "git-baseline.tar")
	writeTestFile(t, snapshot, "PRIVATE SNAPSHOT WITHOUT DATASET PATCH SENTINELS")
	writeTestFile(t, ignoredSnapshot, "PINNED IGNORED BUILD BASELINE")
	writeTestFile(t, gitSnapshot, "PINNED SANITIZED GIT BASELINE")
	details := taskDetails{
		baseCommit:      strings.Repeat("a", 40),
		testPatch:       fixtureTestPatch,
		failToPass:      []string{"tests.regression_test::test_fix"},
		passToPass:      []string{"tests.existing_test::test_ok"},
		selectedTests:   []string{"tests/regression_test.py"},
		verifierFiles:   []string{"tests/regression_test.py"},
		runScript:       runScript,
		parser:          parser,
		snapshot:        snapshot,
		ignoredSnapshot: ignoredSnapshot,
		gitSnapshot:     gitSnapshot,
	}
	benchmark := &Benchmark{
		root: root, outputDir: output, datasetRevision: datasetRevision, evaluatorRevision: evaluatorRevision,
		details: map[string]taskDetails{fixtureInstanceID: details},
	}
	return benchmark, core.Task{ID: fixtureInstanceID}, details
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
