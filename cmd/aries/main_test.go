package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
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
			experiment, err := buildExperiment(cfg, "test-run", cfg.OutputDir)
			if test.wantErr == "" {
				if err != nil || experiment == nil {
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

type counterReader struct {
	next uint64
}

func (reader *counterReader) Read(content []byte) (int, error) {
	clear(content)
	binary.BigEndian.PutUint64(content[len(content)-8:], reader.next)
	reader.next++
	return len(content), nil
}
