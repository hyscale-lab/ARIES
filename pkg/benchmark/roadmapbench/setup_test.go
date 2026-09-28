package roadmapbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestSetupSparseCheckoutIncludesOnlyRuntimeInputs(t *testing.T) {
	source := writeFixture(t)
	for index := range 16 {
		writeTaskFixture(t, source, fmt.Sprintf("task-%03d", index), fixtureTaskTOML)
	}
	commitFixture(t, source)
	fixtureGit(t, source, "config", "uploadpack.allowFilter", "true")
	root := filepath.Join(t.TempDir(), "roadmapbench")
	revision := fixtureGitRevision(source)
	if err := Setup(context.Background(), root, "file://"+source, revision); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"task.toml", "instruction.md", "environment/Dockerfile", "tests/test.sh", "tests/test_outputs.py", "tests/nested/hidden.patch"} {
		if _, err := os.Stat(filepath.Join(root, fixtureTaskID, path)); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	for _, path := range []string{"environment/repo", "solution"} {
		if _, err := os.Lstat(filepath.Join(root, fixtureTaskID, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("populated %s: %v", path, err)
		}
	}
	if got := fixtureGit(t, root, "config", "remote.origin.partialclonefilter"); got != "blob:none" {
		t.Fatalf("filter = %q", got)
	}
	if got := fixtureGit(t, root, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("shallow = %q", got)
	}
	if _, _, err := loadTask(root, fixtureTaskID); err != nil {
		t.Fatal(err)
	}
	for index := range 16 {
		id := fmt.Sprintf("task-%03d", index)
		if _, _, err := loadTask(root, id); err != nil {
			t.Fatalf("task %q across sparse batches: %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(root, id, "environment", "repo")); !os.IsNotExist(err) {
			t.Fatalf("task %q vendored repository was populated: %v", id, err)
		}
	}
	if err := Setup(context.Background(), root, "file://"+source, revision); err != nil {
		t.Fatalf("idempotent Setup: %v", err)
	}
	for _, path := range []string{"task.toml", "instruction.md", "environment/Dockerfile", "tests/test.sh", "tests/nested/hidden.patch"} {
		t.Run(path, func(t *testing.T) {
			file := filepath.Join(root, fixtureTaskID, path)
			original, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, file, "changed runtime input\n")
			if err := VerifyRevision(context.Background(), root, revision); err == nil || !strings.Contains(err.Error(), "local changes") {
				t.Fatalf("sparse dirty check for %s = %v", path, err)
			}
			if err := os.WriteFile(file, original, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSetupBoundsSparseBlobRequests(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
case "$3" in
  ls-tree)
    index=0
    while [ "$index" -lt 33 ]; do
      printf 'task-%03d\000' "$index"
      index=$((index + 1))
    done
    ;;
  sparse-checkout)
    shift 3
    case "$1" in
      set|add)
        shift
        if [ "$1" = "--no-cone" ]; then shift; fi
        # The public promisor remote rejects requests expanded across all
        # task roots. Reproduce that limit without network in this test.
        case "$*" in *'/*/'*) echo 'HTTP 500: unbounded sparse request' >&2; exit 1;; esac
        if [ "$#" -gt 4 ]; then echo 'HTTP 500: too many sparse inputs' >&2; exit 1; fi
        printf 'batch\n' >> "$ARIES_SPARSE_BATCHES"
        ;;
    esac
    ;;
  rev-parse) printf '%s\n' "$ARIES_GIT_REVISION" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	parent := t.TempDir()
	batches := filepath.Join(parent, "batches")
	t.Setenv("ARIES_SPARSE_BATCHES", batches)
	revision := strings.Repeat("b", 40)
	t.Setenv("ARIES_GIT_REVISION", revision)
	if err := Setup(context.Background(), filepath.Join(parent, "roadmapbench"), "https://example.invalid/roadmapbench.git", revision); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(batches)
	if err != nil || strings.Count(string(output), "batch\n") != 33 {
		t.Fatalf("bounded sparse batches = %q, %v", output, err)
	}
}

func TestVerifyRevisionRejectsIgnoredUnpinnedVerifier(t *testing.T) {
	root := writeFixture(t)
	revision := fixtureGitRevision(root)
	writeFile(t, filepath.Join(root, ".git", "info", "exclude"), "ignored.py\n")
	writeFile(t, filepath.Join(root, fixtureTaskID, "tests", "ignored.py"), "unpinned verifier\n")
	if status := fixtureGit(t, root, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("ignored-file premise failed: %s", status)
	}
	if err := VerifyRevision(context.Background(), root, revision); err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("ignored verifier accepted: %v", err)
	}
}

func TestSetupRejectsUnsafeTaskDirectoryNames(t *testing.T) {
	source := writeFixture(t)
	writeTaskFixture(t, source, "unsafe task", fixtureTaskTOML)
	commitFixture(t, source)
	fixtureGit(t, source, "config", "uploadpack.allowFilter", "true")
	err := Setup(context.Background(), filepath.Join(t.TempDir(), "roadmapbench"), "file://"+source, fixtureGitRevision(source))
	if err == nil || !strings.Contains(err.Error(), "invalid roadmapbench task directory") {
		t.Fatalf("unsafe task directory accepted: %v", err)
	}
}

func TestSetupRejectsWrongDirtyAndInvalidDestinations(t *testing.T) {
	root := writeFixture(t)
	revision := fixtureGitRevision(root)
	if err := Setup(context.Background(), root, "unused", strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "want pinned") {
		t.Fatalf("wrong revision error = %v", err)
	}
	writeFile(t, filepath.Join(root, fixtureTaskID, "tests/test.sh"), "changed\n")
	if err := Setup(context.Background(), root, "unused", revision); err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("dirty root error = %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, fixtureTaskID, "tests/test.sh")); err != nil || string(content) != "changed\n" {
		t.Fatalf("existing root altered: %q, %v", content, err)
	}
	for _, unsafe := range []string{"", ".", "/"} {
		if err := Setup(context.Background(), unsafe, "unused", revision); err == nil {
			t.Fatalf("accepted root %q", unsafe)
		}
	}
	if err := Setup(context.Background(), filepath.Join(t.TempDir(), "new"), "", revision); err == nil {
		t.Fatal("accepted missing URL")
	}
	if err := Setup(context.Background(), filepath.Join(t.TempDir(), "new"), "unused", ""); err == nil {
		t.Fatal("accepted missing revision")
	}
}

func TestSetupCleansTemporaryCheckoutAfterGitFailures(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
count=0
if [ -f "$ARIES_GIT_COUNT" ]; then count=$(cat "$ARIES_GIT_COUNT"); fi
count=$((count + 1))
printf '%s' "$count" > "$ARIES_GIT_COUNT"
if [ "$count" = "$ARIES_GIT_FAIL_AT" ]; then
  echo "injected setup interruption" >&2
  exit 42
fi
case "$*" in
  *"rev-parse HEAD"*) printf '%s\n' "$ARIES_GIT_REVISION" ;;
  *"ls-tree -z -d --name-only FETCH_HEAD"*) printf 'fixture-task\000' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARIES_GIT_REVISION", strings.Repeat("b", 40))
	for stage := 1; stage <= 11; stage++ {
		t.Run(strconv.Itoa(stage), func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "roadmapbench")
			t.Setenv("ARIES_GIT_COUNT", filepath.Join(parent, "count"))
			t.Setenv("ARIES_GIT_FAIL_AT", strconv.Itoa(stage))
			stale := filepath.Join(parent, ".roadmapbench-setup-stale")
			if err := os.Mkdir(stale, 0o755); err != nil {
				t.Fatal(err)
			}
			err := Setup(context.Background(), root, "https://example.invalid/roadmapbench.git", strings.Repeat("b", 40))
			if err == nil || !strings.Contains(err.Error(), "injected setup interruption") {
				t.Fatalf("Setup error = %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("partial destination survived: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(parent, ".roadmapbench-setup-*"))
			if err != nil || len(matches) != 1 || matches[0] != stale {
				t.Fatalf("temporary checkouts = %v, %v", matches, err)
			}
		})
	}
}

func TestConcurrentSetupConvergesAndCanceledInstallDoesNotPublish(t *testing.T) {
	source := writeFixture(t)
	fixtureGit(t, source, "config", "uploadpack.allowFilter", "true")
	revision := fixtureGitRevision(source)
	root := filepath.Join(t.TempDir(), "roadmapbench")
	var group sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- Setup(context.Background(), root, "file://"+source, revision)
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyRevision(context.Background(), root, revision); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(root), ".roadmapbench-setup-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary checkouts = %v, %v", matches, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	destination := filepath.Join(t.TempDir(), "not-published")
	if err := installCheckout(canceled, source, destination, revision); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled install = %v", err)
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("canceled install published destination: %v", err)
	}
}

func TestInstallCheckoutRejectsDifferentWinner(t *testing.T) {
	root := writeFixture(t)
	revision := fixtureGitRevision(root)
	candidate := writeFixture(t)
	writeFile(t, filepath.Join(candidate, fixtureTaskID, "instruction.md"), "different revision\n")
	commitFixture(t, candidate)
	if err := installCheckout(context.Background(), candidate, root, fixtureGitRevision(candidate)); err == nil {
		t.Fatal("accepted different winner")
	}
	if err := VerifyRevision(context.Background(), root, revision); err != nil {
		t.Fatalf("winner changed: %v", err)
	}
}
