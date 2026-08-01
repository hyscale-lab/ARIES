package terminalbench

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
			root := filepath.Join(parent, "terminal-bench")
			count := filepath.Join(parent, "count")
			t.Setenv("ARIES_GIT_COUNT", count)
			t.Setenv("ARIES_GIT_FAIL_AT", strconv.Itoa(stage))
			revision := strings.Repeat("a", 40)
			t.Setenv("ARIES_GIT_REVISION", revision)
			stale := filepath.Join(parent, ".terminal-bench-2-setup-stale")
			if err := os.Mkdir(stale, 0o755); err != nil {
				t.Fatal(err)
			}
			err := Setup(context.Background(), root, "https://example.invalid/terminal-bench.git", revision)
			if err == nil || !strings.Contains(err.Error(), "injected setup interruption") {
				t.Fatalf("Setup error = %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("partial destination survived: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(parent, ".terminal-bench-2-setup-*"))
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
	root := filepath.Join(parent, "terminal-bench")
	t.Setenv("ARIES_ROOT", root)
	original := renameCheckout
	renameCheckout = func(string, string) error { return context.Canceled }
	t.Cleanup(func() { renameCheckout = original })
	if err := Setup(context.Background(), root, "https://example.invalid/terminal-bench.git", revision); !strings.Contains(err.Error(), "install terminalbench") {
		t.Fatalf("Setup rename error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(parent, ".terminal-bench-2-setup-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary checkout survived rename failure: %v, %v", matches, err)
	}
}
