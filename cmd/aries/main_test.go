package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	openclawharness "github.com/hyscale-lab/aries/pkg/harness/openclaw"
)

func TestBuildExperimentUsesExplicitTypeSwitches(t *testing.T) {
	valid := config.Config{
		Benchmark: config.BenchmarkConfig{Type: "terminalbench2", Root: terminalbench.DefaultRoot, Tasks: []string{"fix-git"}},
		Harness:   config.HarnessConfig{Type: "openclaw", Image: "ghcr.io/openclaw/openclaw:2026.5.26@sha256:ae7ff536446f1bbb57ea51b9b21097d8f299d30d683dcd72644973bc0522f3b3"},
		Sandbox:   config.SandboxConfig{Type: "docker"},
		Bridge:    config.BridgeConfig{Type: "openclaw-ssh"},
		Model:     config.ModelConfig{BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY"},
		OutputDir: t.TempDir(),
	}

	tests := []struct {
		name    string
		change  func(*config.Config)
		wantErr string
	}{
		{"known", func(*config.Config) {}, ""},
		{"benchmark", func(c *config.Config) { c.Benchmark.Type = "other" }, `unsupported benchmark type "other"`},
		{"harness", func(c *config.Config) { c.Harness.Type = "other" }, `unsupported harness type "other"`},
		{"sandbox", func(c *config.Config) { c.Sandbox.Type = "other" }, `unsupported sandbox type "other"`},
		{"bridge", func(c *config.Config) { c.Bridge.Type = "other" }, `unsupported bridge type "other"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.change(&cfg)
			experiment, err := buildExperiment(cfg, "test-run", cfg.OutputDir, nil)
			if test.wantErr == "" {
				if err != nil || experiment == nil || experiment.Runner == nil || experiment.Recorder == nil {
					t.Fatalf("buildExperiment() = %v, %v", experiment, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("buildExperiment() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestNewRunIDIsUniqueAndSafe(t *testing.T) {
	now := time.Date(2026, 7, 21, 2, 3, 4, 5, time.FixedZone("other", 3*60*60))
	wantPattern := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,127}$`)
	seen := make(map[string]struct{}, 128)
	random := &counterReader{}
	for range 128 {
		id, err := newRunID(now, random)
		if err != nil {
			t.Fatal(err)
		}
		if !wantPattern.MatchString(id) || strings.ContainsAny(id, `/\\`) {
			t.Fatalf("unsafe run ID %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate run ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestCreateAndPersistRunResultUsesPrivateNoReplaceFiles(t *testing.T) {
	outputRoot := filepath.Join(t.TempDir(), "runs", "run-id")
	if err := createRunOutputRoot(outputRoot); err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(outputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if rootInfo.Mode().Perm() != 0o700 {
		t.Fatalf("output root mode = %04o, want 0700", rootInfo.Mode().Perm())
	}

	resultPath := filepath.Join(outputRoot, runResultName)
	if err := persistRunResult(resultPath, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	resultInfo, err := os.Stat(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if resultInfo.Mode().Perm() != 0o600 {
		t.Fatalf("result mode = %04o, want 0600", resultInfo.Mode().Perm())
	}
	if err := persistRunResult(resultPath, []byte("second\n")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("persist stale result error = %v, want os.ErrExist", err)
	}
	content, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "first\n" {
		t.Fatalf("stale result was replaced: %q", content)
	}
	entries, err := os.ReadDir(outputRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != runResultName {
		t.Fatalf("atomic temporary file leaked: %v", entries)
	}
}

func TestExecuteAndRecordPreservesRunAndPersistenceErrorsAndWritesResult(t *testing.T) {
	outputRoot := t.TempDir()
	if err := os.Chmod(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(outputRoot, runResultName)
	if err := os.WriteFile(resultPath, []byte("stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantRunErr := errors.New("runner failed after evaluation")
	wantResult := core.RunResult{Name: "experiment", RunID: "run-id", Summary: core.RunSummary{Tasks: 1}}
	var stdout bytes.Buffer

	err := executeAndRecord(context.Background(), func(context.Context) (core.RunResult, error) {
		return wantResult, wantRunErr
	}, outputRoot, &stdout)
	if !errors.Is(err, wantRunErr) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("executeAndRecord() error = %v, want joined run and persistence errors", err)
	}
	var stdoutResult core.RunResult
	if decodeErr := json.Unmarshal(stdout.Bytes(), &stdoutResult); decodeErr != nil {
		t.Fatalf("decode stdout result: %v; content=%q", decodeErr, stdout.String())
	}
	if stdoutResult.Name != wantResult.Name || stdoutResult.RunID != wantResult.RunID || stdoutResult.Summary.Tasks != 1 {
		t.Fatalf("stdout result = %#v, want %#v", stdoutResult, wantResult)
	}
	stale, readErr := os.ReadFile(resultPath)
	if readErr != nil || string(stale) != "stale\n" {
		t.Fatalf("stale result changed: content=%q error=%v", stale, readErr)
	}
}

func TestExecuteAndRecordPersistsResultWhenRunFails(t *testing.T) {
	outputRoot := t.TempDir()
	if err := os.Chmod(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	wantRunErr := errors.New("harness failed but evaluation completed")
	wantResult := core.RunResult{Name: "experiment", RunID: "run-id", Summary: core.RunSummary{EvaluationsRun: 1}}
	var stdout bytes.Buffer

	err := executeAndRecord(context.Background(), func(context.Context) (core.RunResult, error) {
		return wantResult, wantRunErr
	}, outputRoot, &stdout)
	if !errors.Is(err, wantRunErr) {
		t.Fatalf("executeAndRecord() error = %v, want original run error", err)
	}
	disk, readErr := os.ReadFile(filepath.Join(outputRoot, runResultName))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(disk, stdout.Bytes()) {
		t.Fatalf("disk and stdout results differ: disk=%q stdout=%q", disk, stdout.Bytes())
	}
	var persisted core.RunResult
	if decodeErr := json.Unmarshal(disk, &persisted); decodeErr != nil {
		t.Fatalf("decode persisted result: %v", decodeErr)
	}
	if persisted.Name != wantResult.Name || persisted.RunID != wantResult.RunID || persisted.Summary.EvaluationsRun != 1 {
		t.Fatalf("persisted result = %#v, want %#v", persisted, wantResult)
	}
}

func TestRunRejectsUnknownSetupComponent(t *testing.T) {
	err := run([]string{"setup", "other"})
	if err == nil || !strings.Contains(err.Error(), `unsupported setup component "other"`) {
		t.Fatalf("run() error = %v", err)
	}
}

func TestRunCommandUsesFixedLocalKeyAndPersistsSanitizedLoaderFailure(t *testing.T) {
	repositoryRoot, executablePath := createTestAriesRepository(t)
	secret := "synthetic-command-key"
	keyPath := filepath.Join(repositoryRoot, localAPIKeyFile)
	if err := os.WriteFile(keyPath, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(t.TempDir(), "runs")
	configPath := writeCommandConfig(t, runs)
	var stdout bytes.Buffer
	err := runCommandWithDependencies(context.Background(), []string{configPath}, &stdout, commandDependencies{executablePath: executablePath})
	if err == nil || strings.Contains(err.Error(), secret) || stdout.Len() != 0 {
		t.Fatalf("runCommand() error=%v stdout=%q", err, stdout.String())
	}
	validation := readOnlyLiveValidation(t, runs)
	if validation.Status != liveValidationFailed || validation.Category != liveValidationCredentialInvalid || validation.Attempts != 0 {
		t.Fatalf("validation = %+v", validation)
	}
}

func TestRunCommandFallsBackToEnvironmentWithoutMutatingIt(t *testing.T) {
	_, executablePath := createTestAriesRepository(t)
	launchDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(launchDirectory, localAPIKeyFile), []byte("synthetic-cwd-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	restoreWorkingDirectory(t, launchDirectory)
	value := "synthetic-environment-key\ninvalid"
	t.Setenv(deepSeekAPIKey, value)
	runs := filepath.Join(t.TempDir(), "runs")
	configPath := writeCommandConfig(t, runs)
	doer := &preflightDoer{t: t}
	err := runCommandWithDependencies(context.Background(), []string{configPath}, io.Discard, commandDependencies{
		executablePath: executablePath, preflightClient: doer,
	})
	if err == nil || strings.Contains(err.Error(), value) {
		t.Fatalf("runCommand() error = %v", err)
	}
	if doer.requests != 0 {
		t.Fatalf("preflight made %d requests instead of rejecting the environment fallback", doer.requests)
	}
	if got := os.Getenv(deepSeekAPIKey); got != value {
		t.Fatalf("environment value changed: %q", got)
	}
	validation := readOnlyLiveValidation(t, runs)
	if validation.Status != liveValidationFailed || validation.Category != liveValidationCredentialInvalid || validation.Attempts != 0 {
		t.Fatalf("validation = %+v", validation)
	}
}

func TestRunCommandSelectsAnchoredRepositoryKeyAndIgnoresLaunchCWD(t *testing.T) {
	repositoryRoot, executablePath := createTestAriesRepository(t)
	anchoredKey := "synthetic-anchored-key"
	if err := os.WriteFile(filepath.Join(repositoryRoot, localAPIKeyFile), []byte(anchoredKey), 0o600); err != nil {
		t.Fatal(err)
	}
	launchDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(launchDirectory, localAPIKeyFile), []byte("synthetic-cwd-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	restoreWorkingDirectory(t, launchDirectory)
	runs := filepath.Join(t.TempDir(), "runs")
	configPath := writeCommandConfig(t, runs)
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content = bytes.Replace(content, []byte(`"type":"terminalbench2"`), []byte(`"type":"unsupported"`), 1)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	doer := &preflightDoer{t: t, replies: []preflightReply{{
		status: http.StatusOK, body: `{"data":[{"id":"deepseek-v4-flash"}]}`,
	}}}
	err = runCommandWithDependencies(context.Background(), []string{configPath}, io.Discard, commandDependencies{
		executablePath: executablePath, preflightClient: doer,
		preflightSleep: func(context.Context, time.Duration) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported benchmark type "unsupported"`) {
		t.Fatalf("runCommand() error = %v", err)
	}
	if len(doer.authorizations) != 1 || doer.authorizations[0] != "Bearer "+anchoredKey {
		t.Fatalf("preflight authorization selected the wrong source: %q", doer.authorizations)
	}
	validation := readOnlyLiveValidation(t, runs)
	if validation.Status != liveValidationSucceeded || validation.Category != liveValidationConfirmed || validation.Attempts != 1 {
		t.Fatalf("validation = %+v", validation)
	}
}

func TestRunCommandFailsClosedWhenRepositoryRootIsUntrusted(t *testing.T) {
	launchDirectory := t.TempDir()
	if err := os.WriteFile(filepath.Join(launchDirectory, localAPIKeyFile), []byte("synthetic-cwd-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	restoreWorkingDirectory(t, launchDirectory)
	untrustedExecutable := filepath.Join(t.TempDir(), ariesExecutableName)
	if err := os.WriteFile(untrustedExecutable, []byte("synthetic executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(t.TempDir(), "runs")
	configPath := writeCommandConfig(t, runs)
	doer := &preflightDoer{t: t}
	err := runCommandWithDependencies(context.Background(), []string{configPath}, io.Discard, commandDependencies{
		executablePath: untrustedExecutable, preflightClient: doer,
	})
	if err == nil || !strings.Contains(err.Error(), "resolve ARIES repository root") || doer.requests != 0 {
		t.Fatalf("runCommand() error=%v requests=%d", err, doer.requests)
	}
	validation := readOnlyLiveValidation(t, runs)
	if validation.Status != liveValidationFailed || validation.Category != liveValidationConfigurationInvalid || validation.Attempts != 0 {
		t.Fatalf("validation = %+v", validation)
	}
}

func TestRunCommandLeavesNonDeepSeekOnExistingEnvironmentPath(t *testing.T) {
	root := t.TempDir()
	restoreWorkingDirectory(t, root)
	if err := os.WriteFile(localAPIKeyFile, []byte("invalid-local-file"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(localAPIKeyFile, 0o640); err != nil {
		t.Fatal(err)
	}
	configPath := writeCommandConfig(t, filepath.Join(root, "runs"))
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content = bytes.Replace(content, []byte(`"type":"terminalbench2"`), []byte(`"type":"unsupported"`), 1)
	content = bytes.Replace(content, []byte(`"base_url":"https://api.deepseek.com"`), []byte(`"base_url":"http://fake-model.invalid/v1"`), 1)
	content = bytes.Replace(content, []byte(`"model":"deepseek-v4-flash"`), []byte(`"model":"deterministic"`), 1)
	content = bytes.Replace(content, []byte(`"api_key_env":"DEEPSEEK_API_KEY"`), []byte(`"api_key_env":"ARIES_FAKE_KEY"`), 1)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	err = runCommand(context.Background(), []string{configPath}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), `unsupported benchmark type "unsupported"`) {
		t.Fatalf("runCommand() error = %v", err)
	}
	validation := readOnlyLiveValidation(t, filepath.Join(root, "runs"))
	if validation.Status != liveValidationNotRequested || validation.Category != liveValidationSkipped || validation.Attempts != 0 {
		t.Fatalf("validation = %+v", validation)
	}
}

func restoreWorkingDirectory(t *testing.T, directory string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

func writeCommandConfig(t *testing.T, outputDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "experiment.json")
	content := fmt.Sprintf(`{
  "name":"test-live-validation",
  "benchmark":{"type":"terminalbench2","root":".cache/terminal-bench-2","tasks":["fix-git"]},
  "harness":{"type":"openclaw","image":%q},
  "sandbox":{"type":"docker"},
  "bridge":{"type":"openclaw-ssh"},
  "model":{"base_url":"https://api.deepseek.com","model":"deepseek-v4-flash","api_key_env":"DEEPSEEK_API_KEY"},
  "output_dir":%q
}`, openclawharness.PinnedImage, outputDir)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readOnlyLiveValidation(t *testing.T, runs string) liveValidation {
	t.Helper()
	entries, err := os.ReadDir(runs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("run entries = %v", entries)
	}
	content, err := os.ReadFile(filepath.Join(runs, entries[0].Name(), liveValidationName))
	if err != nil {
		t.Fatal(err)
	}
	var validation liveValidation
	if err := json.Unmarshal(content, &validation); err != nil {
		t.Fatal(err)
	}
	return validation
}

func createTestAriesRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(ariesModuleLine+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, ariesExecutableName)
	if err := os.WriteFile(executable, []byte("synthetic executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	return root, executable
}

type counterReader struct {
	next uint64
}

func (reader *counterReader) Read(content []byte) (int, error) {
	clear(content)
	binary.BigEndian.PutUint64(content[len(content)-8:], reader.next)
	reader.next++
	return len(content), nil
}
