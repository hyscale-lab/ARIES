package sweatlas

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestSetupCleansTemporaryCheckoutAfterEveryGitPreparationFailure(t *testing.T) {
	bin := t.TempDir()
	gitPath := filepath.Join(bin, "git")
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
esac
exit 0
`
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for stage := 1; stage <= 6; stage++ {
		t.Run(strconv.Itoa(stage), func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "swe-atlas-qa")
			count := filepath.Join(parent, "count")
			t.Setenv("ARIES_GIT_COUNT", count)
			t.Setenv("ARIES_GIT_FAIL_AT", strconv.Itoa(stage))
			revision := strings.Repeat("a", 40)
			t.Setenv("ARIES_GIT_REVISION", revision)
			stale := filepath.Join(parent, ".swe-atlas-qa-setup-stale")
			if err := os.Mkdir(stale, 0o755); err != nil {
				t.Fatal(err)
			}
			err := Setup(context.Background(), root, "https://example.invalid/swe-atlas.git", revision)
			if err == nil || !strings.Contains(err.Error(), "injected setup interruption") {
				t.Fatalf("Setup error = %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("partial destination survived: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(parent, ".swe-atlas-qa-setup-*"))
			if err != nil || len(matches) != 1 || matches[0] != stale {
				t.Fatalf("temporary checkouts survived: %v, %v", matches, err)
			}
		})
	}
}

func TestSetupCleansTemporaryCheckoutAfterRenameFailure(t *testing.T) {
	bin := t.TempDir()
	gitPath := filepath.Join(bin, "git")
	revision := strings.Repeat("b", 40)
	script := `#!/bin/sh
case "$*" in *"$ARIES_ROOT"*) exit 3 ;; esac
case "$*" in *"rev-parse HEAD"*) printf '%s\n' "$ARIES_GIT_REVISION" ;; esac
exit 0
`
	if err := os.WriteFile(gitPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ARIES_GIT_REVISION", revision)
	parent := t.TempDir()
	root := filepath.Join(parent, "swe-atlas-qa")
	t.Setenv("ARIES_ROOT", root)
	original := renameCheckout
	renameCheckout = func(string, string) error { return context.Canceled }
	t.Cleanup(func() { renameCheckout = original })
	if err := Setup(context.Background(), root, "https://example.invalid/swe-atlas.git", revision); !strings.Contains(err.Error(), "install sweatlas") {
		t.Fatalf("Setup rename error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(parent, ".swe-atlas-qa-setup-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary checkout survived rename failure: %v, %v", matches, err)
	}
}

func TestSetupRejectsWrongExistingRevisionWithoutReplacingIt(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "marker")
	if err := os.WriteFile(marker, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		commandArgs := append([]string{"-C", root}, args...)
		command := exec.Command("git", commandArgs...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	git("init", "--quiet")
	git("add", "marker")
	git("-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "commit", "--quiet", "-m", "fixture")

	err := Setup(context.Background(), root, "https://example.invalid/swe-atlas.git", fixtureRevision)
	if err == nil || !strings.Contains(err.Error(), "want pinned") {
		t.Fatalf("Setup() error = %v, want wrong revision", err)
	}
	content, readErr := os.ReadFile(marker)
	if readErr != nil || string(content) != "keep\n" {
		t.Fatalf("existing checkout was altered: content=%q error=%v", content, readErr)
	}
}

func TestInstallCheckoutConcurrentSameRevisionConverges(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run(source, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(source, "fixture"), []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(source, "add", "fixture")
	run(source, "-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "commit", "--quiet", "-m", "fixture")
	revision := run(source, "rev-parse", "HEAD")

	temporary := make([]string, 2)
	for index := range temporary {
		temporary[index] = filepath.Join(parent, "candidate-"+strconv.Itoa(index))
		output, err := exec.Command("git", "clone", "--quiet", source, temporary[index]).CombinedOutput()
		if err != nil {
			t.Fatalf("clone candidate %d: %v: %s", index, err, output)
		}
		run(temporary[index], "checkout", "--quiet", "--detach", revision)
	}

	root := filepath.Join(parent, "swe-atlas-qa")
	start := make(chan struct{})
	errs := make(chan error, len(temporary))
	var ready sync.WaitGroup
	ready.Add(len(temporary))
	for _, candidate := range temporary {
		go func(candidate string) {
			ready.Done()
			<-start
			errs <- installCheckout(context.Background(), candidate, root, revision)
		}(candidate)
	}
	ready.Wait()
	close(start)
	for range temporary {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent install: %v", err)
		}
	}
	if err := VerifyRevision(context.Background(), root, revision); err != nil {
		t.Fatalf("installed checkout: %v", err)
	}
}
