package swebenchpro

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestSetupInstallsAtomicPinnedPair(t *testing.T) {
	dataset, datasetRevision := setupRepositoryFixture(t, map[string]string{
		datasetParquetPath: "PAR1fixture dataset\n",
	})
	evaluator, evaluatorRevision := setupRepositoryFixture(t, map[string]string{
		"run_scripts/fixture/run_script.sh": "#!/bin/sh\n",
	})
	root := filepath.Join(t.TempDir(), "swe-bench-pro")

	if err := Setup(context.Background(), root, dataset, datasetRevision, evaluator, evaluatorRevision); err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if err := verifySetupRoot(context.Background(), root, datasetRevision, evaluatorRevision); err != nil {
		t.Fatalf("installed root: %v", err)
	}
	for _, checkout := range []string{"dataset", "evaluator"} {
		branch := setupGit(t, filepath.Join(root, checkout), "rev-parse", "--abbrev-ref", "HEAD")
		if branch != "HEAD" {
			t.Fatalf("%s checkout branch = %q, want detached HEAD", checkout, branch)
		}
	}
	if err := Setup(context.Background(), root, dataset, datasetRevision, evaluator, evaluatorRevision); err != nil {
		t.Fatalf("idempotent Setup() error = %v", err)
	}
}

func TestSetupDoesNotPublishPartialRoot(t *testing.T) {
	dataset, datasetRevision := setupRepositoryFixture(t, map[string]string{
		datasetParquetPath: "PAR1fixture dataset\n",
	})
	root := filepath.Join(t.TempDir(), "swe-bench-pro")
	err := Setup(context.Background(), root, dataset, datasetRevision, filepath.Join(t.TempDir(), "missing"), strings.Repeat("1", 40))
	if err == nil {
		t.Fatal("Setup() succeeded with missing evaluator repository")
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("partially installed root exists: %v", statErr)
	}
}

func TestSetupRejectsUnsafeAndInvalidExistingRoots(t *testing.T) {
	if err := Setup(context.Background(), ".", "dataset", "dataset-revision", "evaluator", "evaluator-revision"); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("Setup(.) error = %v, want unsafe root", err)
	}
	file := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(file, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(context.Background(), file, "dataset", "dataset-revision", "evaluator", "evaluator-revision"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Setup(file) error = %v, want non-directory", err)
	}
}

func TestSetupRejectsWrongOrDirtyExistingPairWithoutReplacingIt(t *testing.T) {
	dataset, datasetRevision := setupRepositoryFixture(t, map[string]string{
		datasetParquetPath: "PAR1fixture dataset\n",
	})
	evaluator, evaluatorRevision := setupRepositoryFixture(t, map[string]string{
		"parser.py": "print('fixture')\n",
	})
	root := filepath.Join(t.TempDir(), "swe-bench-pro")
	if err := Setup(context.Background(), root, dataset, datasetRevision, evaluator, evaluatorRevision); err != nil {
		t.Fatal(err)
	}

	wrongRevision := strings.Repeat("f", 40)
	if wrongRevision == datasetRevision {
		wrongRevision = strings.Repeat("e", 40)
	}
	if err := Setup(context.Background(), root, dataset, wrongRevision, evaluator, evaluatorRevision); err == nil || !strings.Contains(err.Error(), "want pinned") {
		t.Fatalf("wrong revision error = %v", err)
	}
	marker := filepath.Join(root, "dataset", "untracked")
	if err := os.WriteFile(marker, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Setup(context.Background(), root, dataset, datasetRevision, evaluator, evaluatorRevision); err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("dirty root error = %v", err)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "keep\n" {
		t.Fatalf("existing root was altered: content=%q error=%v", content, err)
	}
}

func TestSetupRejectsUnresolvedDatasetLFSPointer(t *testing.T) {
	dataset, datasetRevision := setupRepositoryFixture(t, map[string]string{
		datasetParquetPath: gitLFSPointerPrefix + "/v1\noid sha256:" + strings.Repeat("0", 64) + "\nsize 123\n",
	})
	evaluator, evaluatorRevision := setupRepositoryFixture(t, map[string]string{"parser.py": "pass\n"})
	root := filepath.Join(t.TempDir(), "swe-bench-pro")

	err := Setup(context.Background(), root, dataset, datasetRevision, evaluator, evaluatorRevision)
	if err == nil || (!strings.Contains(err.Error(), "Git LFS") && !strings.Contains(err.Error(), "git [lfs pull]")) {
		t.Fatalf("Setup() error = %v, want unresolved LFS failure", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("root exists after LFS failure: %v", statErr)
	}
}

func TestDatasetParquetPointerCheckReadsOnlyPrefix(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, filepath.FromSlash(datasetParquetPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{'P'}, 128*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	pointer, err := datasetParquetIsLFSPointer(root)
	if err != nil {
		t.Fatalf("datasetParquetIsLFSPointer() error = %v", err)
	}
	if pointer {
		t.Fatal("binary parquet was classified as a Git LFS pointer")
	}
}

func TestSetupConcurrentSamePairConverges(t *testing.T) {
	dataset, datasetRevision := setupRepositoryFixture(t, map[string]string{
		datasetParquetPath: "PAR1fixture dataset\n",
	})
	evaluator, evaluatorRevision := setupRepositoryFixture(t, map[string]string{"parser.py": "pass\n"})
	root := filepath.Join(t.TempDir(), "swe-bench-pro")

	const installers = 4
	start := make(chan struct{})
	errors := make(chan error, installers)
	var ready sync.WaitGroup
	ready.Add(installers)
	for range installers {
		go func() {
			ready.Done()
			<-start
			errors <- Setup(context.Background(), root, dataset, datasetRevision, evaluator, evaluatorRevision)
		}()
	}
	ready.Wait()
	close(start)
	for range installers {
		if err := <-errors; err != nil {
			t.Fatalf("concurrent Setup() error = %v", err)
		}
	}
	if err := verifySetupRoot(context.Background(), root, datasetRevision, evaluatorRevision); err != nil {
		t.Fatalf("concurrent winner: %v", err)
	}
}

func TestVerifyRevisionRequiresExactCleanHead(t *testing.T) {
	repository, revision := setupRepositoryFixture(t, map[string]string{"fixture": "one\n"})
	if err := VerifyRevision(context.Background(), repository, revision); err != nil {
		t.Fatalf("VerifyRevision(clean) = %v", err)
	}
	if err := VerifyRevision(context.Background(), repository, strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "want pinned") {
		t.Fatalf("VerifyRevision(wrong) = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, "fixture"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRevision(context.Background(), repository, revision); err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("VerifyRevision(dirty) = %v", err)
	}
}

func setupRepositoryFixture(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	setupGit(t, root, "init", "--quiet")
	for path, content := range files {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setupGit(t, root, "add", "--all")
	setupGit(t, root, "-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "commit", "--quiet", "-m", "fixture")
	return root, setupGit(t, root, "rev-parse", "HEAD")
}

func setupGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	output, err := exec.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestInstallSetupRootRejectsDifferentWinner(t *testing.T) {
	dataset, datasetRevision := setupRepositoryFixture(t, map[string]string{datasetParquetPath: "PAR1dataset\n"})
	evaluator, evaluatorRevision := setupRepositoryFixture(t, map[string]string{"parser.py": "pass\n"})
	parent := t.TempDir()
	winner := filepath.Join(parent, "winner")
	if err := Setup(context.Background(), winner, dataset, datasetRevision, evaluator, evaluatorRevision); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(parent, "candidate")
	if err := os.MkdirAll(candidate, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := installSetupRoot(context.Background(), candidate, winner, strings.Repeat("b", 40), evaluatorRevision); err == nil {
		t.Fatal("installSetupRoot accepted a different winner")
	}
	if err := verifySetupRoot(context.Background(), winner, datasetRevision, evaluatorRevision); err != nil {
		t.Fatalf("different installer altered winner: %v", err)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate unexpectedly removed: %v", err)
	}
}
