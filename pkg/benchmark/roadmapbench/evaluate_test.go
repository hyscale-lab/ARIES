package roadmapbench

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

func TestEvaluatePreservesOfficialPartialRewardWithoutCTRF(t *testing.T) {
	for _, reward := range []string{"0", "0.7142857142857143", "1.0"} {
		t.Run(reward, func(t *testing.T) {
			b, task := evaluationFixture(t)
			sandbox := &evaluationSandbox{reward: reward, details: `{"reward":` + reward + `,"pass_weighted":5,"total_weighted":7}`}
			got, err := b.Evaluate(context.Background(), task, sandbox)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]float64{"0": 0, "0.7142857142857143": 5.0 / 7, "1.0": 1}[reward]
			if got.Score != want || got.Reward != want || got.Error != "" {
				t.Fatalf("evaluation = %+v", got)
			}
			status := core.StatusFailed
			if want == 1 {
				status = core.StatusSucceeded
			}
			if got.Status != status || got.VerifierStatus != status {
				t.Fatalf("evaluation statuses = %+v", got)
			}
			if !reflect.DeepEqual(sandbox.uploads, []string{"/tests/test.sh", "/tests/nested/test_target.py"}) {
				t.Fatalf("uploaded files = %v", sandbox.uploads)
			}
			if !reflect.DeepEqual(sandbox.downloads, []string{"/logs/verifier/reward.txt", "/logs/verifier/reward.json"}) {
				t.Fatalf("downloaded files = %v", sandbox.downloads)
			}
			command := sandbox.commands[len(sandbox.commands)-1]
			if command.Path != "/bin/bash" || !reflect.DeepEqual(command.Args, []string{"/tests/test.sh"}) || command.Dir != "/app" || command.Timeout != 30*time.Minute || command.Env["TEST_SETTING"] != "literal; $value" {
				t.Fatalf("verifier invocation = %#v", command)
			}
			if len(got.LogPaths) != 4 {
				t.Fatalf("artifacts = %v", got.LogPaths)
			}
			for _, file := range got.LogPaths {
				info, err := os.Stat(file)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("private artifact %q: %v, %v", file, info, err)
				}
			}
			contents, err := os.ReadFile(filepath.Join(b.outputDir, task.ID, "evaluation", "reward.json"))
			if err != nil || string(contents) != sandbox.details {
				t.Fatalf("official details not preserved: %q, %v", contents, err)
			}
		})
	}
}

func TestEvaluateRejectsInvalidRewardsAndVerifierFailures(t *testing.T) {
	for _, reward := range []string{"", "NaN", "Inf", "-0.1", "1.01", "0.5\n1", "garbage"} {
		t.Run(reward, func(t *testing.T) {
			b, task := evaluationFixture(t)
			got, err := b.Evaluate(context.Background(), task, &evaluationSandbox{reward: reward})
			if err == nil || got.Reward != 0 || got.Status != core.StatusFailed {
				t.Fatalf("accepted malformed reward %q: %+v, %v", reward, got, err)
			}
		})
	}
	for _, sandbox := range []*evaluationSandbox{
		{reward: "1", exitCode: 1},
		{reward: "1", execErr: context.DeadlineExceeded},
		{reward: "1", downloadErr: errors.New("daemon unavailable")},
		{reward: "1", detailsErr: errors.New("details download interrupted")},
		{reward: "1", uploadErr: errors.New("upload failed")},
	} {
		b, task := evaluationFixture(t)
		got, err := b.Evaluate(context.Background(), task, sandbox)
		if err == nil || got.Reward != 0 || got.Error == "" {
			t.Fatalf("verifier fault became a task score: %+v, %v", got, err)
		}
	}
}

func TestEvaluateAllowsAbsentOptionalDetailsAndReverifiesPrivateSources(t *testing.T) {
	b, task := evaluationFixture(t)
	got, err := b.Evaluate(context.Background(), task, &evaluationSandbox{reward: "0.5"})
	if err != nil || got.Score != 0.5 || len(got.LogPaths) != 3 {
		t.Fatalf("reward.txt-only evaluation = %+v, %v", got, err)
	}
	if err := os.WriteFile(b.details[task.ID].verifierFiles[0].source, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sandbox := &evaluationSandbox{reward: "1"}
	if _, err := b.Evaluate(context.Background(), task, sandbox); err == nil {
		t.Fatal("dirty pinned verifier was accepted")
	}
	if len(sandbox.uploads) != 0 || len(sandbox.commands) != 0 {
		t.Fatal("dirty source reached the sandbox")
	}
}

func TestEvaluateResetsStaleUpstreamOutputsForExecutionOccurrences(t *testing.T) {
	for _, check := range []struct{ id, output string }{
		{"opt-3.0.0-roadmap", "/tmp/opt_test_results"},
		{"glz-6.3.0-roadmap", "/tmp/test_06"},
		{"glz-4.0.0-roadmap", "/app/build/tests/phase_tests/test_05_atomic_and_fixes"},
		{"glz-6.2.0-roadmap", "/app/build/tests/benchmark_tests/test_07_buffer_pool"},
		{"glz-6.5.1-roadmap", "/app/build/tests/benchmark_test/test_04_raw_pointers"},
		{"glz-7.0.1-roadmap", "/app/build_bench/bench_test_04"},
		{"ruf-0.3.0-roadmap", "/app/target/debug/ruff"},
		{"ruf-0.12.0-roadmap", "/app/target/debug/ruff"},
	} {
		t.Run(check.id, func(t *testing.T) {
			b, task := evaluationFixture(t)
			details := b.details[task.ID]
			details.taskID = check.id
			task.ID = check.id + "-003"
			b.details[task.ID] = details
			sandbox := &evaluationSandbox{reward: "0"}
			if _, err := b.Evaluate(context.Background(), task, sandbox); err != nil {
				t.Fatal(err)
			}
			for _, command := range sandbox.commands[:2] {
				if !containsArgument(command.Args, check.output) {
					t.Fatalf("stale %s not cleared/confirmed: %#v", check.output, command)
				}
				for _, baseline := range []string{"/tmp", "/app", "/app/build", "/app/target", "/app/target/debug"} {
					if containsArgument(command.Args, baseline) {
						t.Fatalf("cleanup discards baseline caches: %#v", command)
					}
				}
			}
			if check.id == "opt-3.0.0-roadmap" && sandbox.commands[len(sandbox.commands)-1].Env["TMPDIR"] != "/tmp" {
				t.Fatal("Optuna verifier scratch path was not fixed to the cleared directory")
			}
		})
	}
	for _, gate := range []int{1, 2} {
		b, task := evaluationFixture(t)
		sandbox := &evaluationSandbox{reward: "1", failAt: gate}
		if _, err := b.Evaluate(context.Background(), task, sandbox); err == nil || len(sandbox.uploads) != 0 {
			t.Fatalf("failed reset reached private injection: uploads=%v, err=%v", sandbox.uploads, err)
		}
	}
}

func TestPrepareSandboxScrubsAndConfirmsBeforeAnyUpload(t *testing.T) {
	b, task := evaluationFixture(t)
	sandbox := &evaluationSandbox{}
	if err := b.PrepareSandbox(context.Background(), task, sandbox); err != nil {
		t.Fatal(err)
	}
	if len(sandbox.commands) != 2 || len(sandbox.uploads) != 0 {
		t.Fatalf("preparation = %#v; uploads = %v", sandbox.commands, sandbox.uploads)
	}
	for _, private := range []string{"/tests", "/logs/verifier", "/solution", "/tmp/changes.patch", "/opt/polars-upgrade"} {
		for _, command := range sandbox.commands {
			if !containsArgument(command.Args, private) {
				t.Fatalf("private path %s missing from preparation command %#v", private, command)
			}
		}
	}
	for _, failure := range []int{1, 2} {
		sandbox = &evaluationSandbox{failAt: failure}
		if err := b.PrepareSandbox(context.Background(), task, sandbox); err == nil {
			t.Fatalf("preparation accepted failed gate %d", failure)
		}
	}
	if err := b.PrepareSandbox(context.Background(), core.Task{ID: "unknown"}, sandbox); err == nil {
		t.Fatal("unloaded task accepted")
	}
}

func TestPreparePolars135RemovesPipCacheOfOracleDownload(t *testing.T) {
	for _, id := range []string{"plr-1.35.0-roadmap", "plr-1.35.0-roadmap-001", "sample-roadmap-001"} {
		t.Run(id, func(t *testing.T) {
			b, task := evaluationFixture(t)
			details := b.details[task.ID]
			details.taskID = strings.TrimSuffix(id, "-001")
			b.details[id] = details
			task.ID = id
			sandbox := &evaluationSandbox{}
			if err := b.PrepareSandbox(context.Background(), task, sandbox); err != nil {
				t.Fatal(err)
			}
			for _, command := range sandbox.commands {
				if got := containsArgument(command.Args, "/root/.cache/pip"); got != strings.HasPrefix(id, "plr-") {
					t.Fatalf("pip cache cleanup for %s: %#v", id, command)
				}
			}
		})
	}
}

func containsArgument(arguments []string, want string) bool {
	for _, argument := range arguments {
		if argument == want {
			return true
		}
	}
	return false
}

type evaluationSandbox struct {
	commands                                    []core.Command
	uploads, downloads                          []string
	reward, details                             string
	exitCode, failAt                            int
	execErr, uploadErr, downloadErr, detailsErr error
}

func (s *evaluationSandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	s.commands = append(s.commands, command)
	if err := ctx.Err(); err != nil {
		return core.CommandResult{}, err
	}
	if s.failAt == len(s.commands) {
		return core.CommandResult{ExitCode: 1}, nil
	}
	if command.Path == "/bin/bash" {
		return core.CommandResult{ExitCode: s.exitCode, Stdout: "verifier stdout\n", Stderr: "verifier stderr\n"}, s.execErr
	}
	return core.CommandResult{}, nil
}

func (s *evaluationSandbox) Upload(_ context.Context, source, destination string) error {
	if _, err := os.ReadFile(source); err != nil {
		return err
	}
	s.uploads = append(s.uploads, destination)
	return s.uploadErr
}

func (s *evaluationSandbox) Download(ctx context.Context, source, destination string) error {
	return s.DownloadLimit(ctx, source, destination, 1<<20)
}

func (s *evaluationSandbox) DownloadLimit(_ context.Context, source, destination string, limit int64) error {
	s.downloads = append(s.downloads, source)
	if s.downloadErr != nil {
		return s.downloadErr
	}
	content := s.reward
	if strings.HasSuffix(source, "reward.json") {
		if s.detailsErr != nil {
			return s.detailsErr
		}
		if s.details == "" {
			return runner.ErrNotFound
		}
		content = s.details
	}
	if int64(len(content)) > limit {
		return errors.New("download exceeds limit")
	}
	return os.WriteFile(destination, []byte(content), 0o600)
}

func evaluationFixture(t *testing.T) (*Benchmark, core.Task) {
	t.Helper()
	root := t.TempDir()
	files := []verifierFile{}
	for _, name := range []string{"test.sh", "nested/test_target.py"} {
		source := filepath.Join(root, "sample-roadmap", "tests", name)
		if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte("private verifier\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, verifierFile{name: name, source: source, destination: "/tests/" + name})
	}
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", root}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet")
	git("add", ".")
	git("-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "commit", "--quiet", "-m", "fixture")
	b := &Benchmark{root: root, revision: git("rev-parse", "HEAD"), outputDir: t.TempDir(), details: map[string]taskDetails{
		"sample-roadmap-001": {taskID: "sample-roadmap", verifierFiles: files, timeout: 30 * time.Minute, verifierEnv: map[string]string{"TEST_SETTING": "literal; $value"}, workdir: "/app"},
	}}
	return b, core.Task{ID: "sample-roadmap-001"}
}
