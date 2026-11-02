package agentplugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var renameCheckout = os.Rename

// Fetch creates an exact shallow detached checkout of a pinned plugin
// repository at root. An existing root is accepted only when it is a clean
// checkout of the pinned revision.
func Fetch(ctx context.Context, root, repositoryURL, revision string) error {
	if repositoryURL == "" || revision == "" {
		return errors.New("plugin repository URL and revision are required")
	}
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) {
		return fmt.Errorf("unsafe plugin checkout root %q", root)
	}
	info, err := os.Stat(root)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("plugin checkout root %q is not a directory", root)
		}
		return verifyRevision(ctx, root, revision)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect plugin checkout root %q: %w", root, err)
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create plugin cache parent: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".plugin-setup-")
	if err != nil {
		return fmt.Errorf("create temporary plugin checkout: %w", err)
	}
	defer os.RemoveAll(temporary)
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", repositoryURL},
		{"fetch", "--depth=1", "origin", revision},
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
	} {
		if err := runGit(ctx, temporary, args...); err != nil {
			return err
		}
	}
	if err := verifyRevision(ctx, temporary, revision); err != nil {
		return err
	}
	if err := renameCheckout(temporary, root); err != nil {
		// Another setup may have installed the same pinned checkout after the
		// absence check. Accept only a freshly reverified destination.
		if verifyErr := verifyRevision(ctx, root, revision); verifyErr != nil {
			return fmt.Errorf("install plugin checkout at %q: %w", root, errors.Join(err, verifyErr))
		}
	}
	return nil
}

func verifyRevision(ctx context.Context, root, revision string) error {
	output, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify plugin checkout %q: %w: %s", root, err, output)
	}
	if got := strings.TrimSpace(string(output)); got != revision {
		return fmt.Errorf("plugin checkout %q is revision %q; want pinned %q", root, got, revision)
	}
	// Every file under the root is staged, so ignored files count as changes:
	// only the pinned revision may determine the package.
	output, err = exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=all", "--ignored").CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect plugin checkout %q: %w: %s", root, err, output)
	}
	if len(output) != 0 {
		return fmt.Errorf("plugin checkout %q has local changes or ignored files; a pinned plugin must be clean", root)
	}
	return nil
}

// runGit never prompts: a missing or private repository fails preparation
// instead of waiting for credentials. An empty GIT_ASKPASS also overrides
// core.askPass, so no helper runs.
func runGit(ctx context.Context, dir string, args ...string) error {
	command := exec.CommandContext(ctx, "git", append([]string{"-c", "credential.interactive=false", "-C", dir}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, output)
	}
	return nil
}
